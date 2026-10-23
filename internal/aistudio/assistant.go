package aistudio

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Assistant is the third generation route: the Build page code assistant. It is
// a distinct upstream RPC sequence, not the Build application proxy.
//
// Fixed upstream evidence: Mag1cFall/AIStudio2API v0.2.13 @
// a784c5193b3ba5c4894d536b2f4cd73e87f99475 internal/aistudio/assistant.go and
// docs/build.md section 8 ("写代码助手通道").
const (
	// assistantInitMethod is the WAA-protected RPC that submits a code-assistant
	// generation task. This is the physical generation submit and the attempt the
	// shared loop audits. Upstream docs/build.md:514-519, 568-590.
	assistantInitMethod = "CodeAssistantOffline"
	// assistantGenerateMethod reads the step-snapshot stream for a submitted task.
	// It carries no WAA proof and is not a second generation submit. Upstream
	// docs/build.md:531-535, 597-600.
	assistantGenerateMethod = "StreamCodeAssistantOfflineGeneration"
	// assistantStreamMethod is the read-side alias used by this module's tests and
	// call sites; it is the same RPC as assistantGenerateMethod.
	assistantStreamMethod = assistantGenerateMethod
	// assistantCreateAppletMethod creates the temporary applet that carries one
	// request. Upstream docs/build.md:552-566.
	assistantCreateAppletMethod = "CreateApplet"
	// assistantCancelMethod cancels a task that did not finish decoding.
	assistantCancelMethod = "CancelCodeAssistantOfflineGeneration"
	// assistantDeleteAppletMethod deletes the temporary applet.
	assistantDeleteAppletMethod = "DeleteApplet"
	// assistantProofField is the CodeAssistantOffline protobuf field that carries
	// the WAA proof (JSON index 1). Upstream docs/build.md:521.
	assistantProofField = 2
	// assistantDefaultThinkingLevel is the code assistant's default high level.
	assistantDefaultThinkingLevel = 3
	// assistantCleanupTimeout bounds the Cancel + Delete cleanup.
	assistantCleanupTimeout = time.Minute
	// assistantCapabilityCode is the ListModels primary capability code that marks
	// a model selectable in the Build code assistant. JSON index 64 is protobuf
	// field 65. Evidence: upstream internal/aistudio/models.go:27 (48:
	// "code_assistant") and docs/protocol.md:279,300.
	assistantCapabilityCode int64 = 48
)

// assistantFunctionToolsImplemented is the single authoritative statement of
// whether this adapter has a verified client-function tool mapping on the
// CodeAssistantOffline wire.
//
// It is false: the fixed upstream mapping table (docs/build.md:558-584) only
// covers contents (field 1), system (field 9) and thinking level (field 19).
// No client function-declaration/tool-choice envelope has been verified. Shared
// Content encoding alone is not proof that function history is accepted here;
// the upstream adapter strips declarations and transcribes history to text.
// Returning false is an implementation-gap statement,
// not a claim that the underlying model cannot call tools.
func assistantFunctionToolsImplemented() bool { return false }

// assistantCapableModel reports whether the per-account catalog marks a model as
// selectable in the Build code assistant. The live ListModels capability code 48
// (protobuf field 65) is the authoritative signal; a bare model ID cannot prove
// it and no model name is hardcoded here.
func assistantCapableModel(model Model) bool {
	return model.Capabilities["capability_code_"+fmt.Sprint(assistantCapabilityCode)]
}

// assistantBaseModelID returns the real base model ID used on the wire, never
// the local assistant/ routing prefix or a models/ prefix.
func assistantBaseModelID(request GenerateRequest) string {
	if base := strings.TrimPrefix(strings.TrimSpace(request.Selection.BaseModel), "models/"); base != "" {
		return base
	}
	return strings.TrimPrefix(strings.TrimSpace(request.Model), "models/")
}

// validateAssistantRequest checks the normalized text wire request before any
// resource creation. Public generation/codec entries first apply the approved
// Assistant-only upstream tool downgrade; leftovers still fail closed.
func validateAssistantRequest(request GenerateRequest) error {
	if err := assistantClientToolRefusal(request); err != nil {
		return err
	}
	for contentIndex, content := range request.Contents {
		for partIndex, part := range content.Parts {
			switch {
			case part.InlineData != nil:
				return fmt.Errorf("%w: assistant attachment upload is not implemented: contents[%d].parts[%d] inline data cannot be represented on the CodeAssistantOffline wire", ErrInvalidArgument, contentIndex, partIndex)
			case part.ExternalMedia != nil:
				return fmt.Errorf("%w: assistant external media has no verified CodeAssistantOffline mapping: contents[%d].parts[%d]", ErrInvalidArgument, contentIndex, partIndex)
			case part.File != nil:
				return fmt.Errorf("%w: assistant Drive file references have no verified CodeAssistantOffline mapping: contents[%d].parts[%d]", ErrInvalidArgument, contentIndex, partIndex)
			}
		}
	}
	if request.Config.MaxOutputTokens != nil {
		return fmt.Errorf("%w: assistant does not implement an explicit max output token budget; CodeAssistantOffline uses server-side generation settings", ErrInvalidArgument)
	}
	if request.Config.Temperature != nil || request.Config.TopP != nil || request.Config.TopK != nil || request.Config.Seed != nil {
		return fmt.Errorf("%w: assistant does not implement temperature, topP, topK or seed; CodeAssistantOffline uses server-side generation settings", ErrInvalidArgument)
	}
	if request.Config.ResponseMIMEType != "" && request.Config.ResponseMIMEType != "text/plain" {
		return fmt.Errorf("%w: assistant does not implement response MIME type %q", ErrInvalidArgument, request.Config.ResponseMIMEType)
	}
	if len(request.Config.ResponseSchema) > 0 {
		return fmt.Errorf("%w: assistant does not implement enforced response schemas", ErrInvalidArgument)
	}
	for _, modality := range request.Config.ResponseModalities {
		if modality != ResponseModalityText {
			return fmt.Errorf("%w: assistant only produces text output", ErrInvalidArgument)
		}
	}
	if request.Config.ImageConfig != nil || request.Config.SpeechConfig != nil || request.Config.TranscriptionConfig != nil {
		return fmt.Errorf("%w: assistant does not implement image, speech or transcription configuration", ErrInvalidArgument)
	}
	if request.Config.MediaResolution != "" {
		return fmt.Errorf("%w: assistant does not implement media resolution", ErrInvalidArgument)
	}
	if len(request.SafetySettings) > 0 {
		return fmt.Errorf("%w: assistant does not implement per-category safety settings; the assistant applies server-side settings", ErrInvalidArgument)
	}
	// Reject an unknown thinking strength before any applet is created.
	if _, err := assistantThinkingLevel(request.Config, GenerationDefaults{}); err != nil {
		return fmt.Errorf("%w: assistant thinking configuration: %v", ErrInvalidArgument, err)
	}
	return nil
}

// assistantThinkingLevel maps the request thinking strength or budget to the
// CodeAssistantOffline thinking level. Unspecified means high (3).
func assistantThinkingLevel(config GenerationConfig, defaults GenerationDefaults) (int64, error) {
	level := int64(assistantDefaultThinkingLevel)
	switch normalizedReasoningEffort(config.ReasoningEffort) {
	case "":
		if config.ThinkingBudget != nil {
			level = thinkingLevelForBudget(*config.ThinkingBudget)
		}
	case "minimal", "none":
		level = 4
	case "low":
		level = 1
	case "medium":
		level = 2
	case "high":
		level = 3
	default:
		return 0, fmt.Errorf("reasoning effort 必须是 none、minimal、low、medium、high、xhigh 或 max")
	}
	return closestSupportedThinkingLevel(level, defaults.ThinkingLevels), nil
}

// EncodeAssistantRequest encodes CodeAssistantOffline. The WAA proof (field 2)
// is written by the protected transport, never here. Evidence:
// docs/build.md:568-590.
func EncodeAssistantRequest(request GenerateRequest, defaults GenerationDefaults, applet string, requestID string) ([]byte, error) {
	// Direct codec calls use the same documented Assistant text downgrade.
	var err error
	request, err = prepareAssistantTextRequest(request)
	if err != nil {
		return nil, err
	}
	if err := validateAssistantRequest(request); err != nil {
		return nil, err
	}
	contents, err := encodeContents(request.Contents)
	if err != nil {
		return nil, err
	}
	if len(contents) == 0 {
		return nil, fmt.Errorf("CodeAssistantOffline contents 不能为空")
	}
	level, err := assistantThinkingLevel(request.Config, defaults)
	if err != nil {
		return nil, err
	}
	wire := make([]any, 29)
	wire[0] = contents                                     // field 1: conversation turns
	wire[5] = int64(1)                                     // field 6
	wire[7] = wireModelName(assistantBaseModelID(request)) // field 8: models/<base model>
	if system := strings.TrimSpace(request.System); system != "" {
		wire[8] = system // field 9: custom instructions
	}
	wire[11] = applet                           // field 12: applet ID
	wire[13] = []any{int64(2), nil, int64(2)}   // field 14
	wire[15] = int64(1)                         // field 16
	wire[18] = []any{int64(1), nil, nil, level} // field 19: thinking config
	wire[28] = requestID                        // field 29: per-request UUID
	return json.Marshal(wire)
}

// assistantBindingPrompt extracts the WAA binding text from an encoded
// CodeAssistantOffline body: each content part contributes its text, inline
// base64 or Drive file ID, joined by single spaces. It mirrors bindingPrompt for
// the Playground GenerateContent binding. Evidence: docs/build.md:592-594.
func assistantBindingPrompt(body []byte) (string, error) {
	var wire []json.RawMessage
	if err := json.Unmarshal(body, &wire); err != nil {
		return "", fmt.Errorf("CodeAssistantOffline 请求不是数组: %w", err)
	}
	if len(wire) == 0 || isJSONNull(wire[0]) {
		return "", fmt.Errorf("CodeAssistantOffline 请求缺少对话轮")
	}
	var contents []json.RawMessage
	if err := json.Unmarshal(wire[0], &contents); err != nil {
		return "", fmt.Errorf("CodeAssistantOffline 对话轮不是数组: %w", err)
	}
	values := make([]string, 0)
	for contentIndex, contentRaw := range contents {
		var content []json.RawMessage
		if err := json.Unmarshal(contentRaw, &content); err != nil || len(content) == 0 {
			return "", fmt.Errorf("CodeAssistantOffline 对话轮 %d 无效", contentIndex)
		}
		var parts []json.RawMessage
		if err := json.Unmarshal(content[0], &parts); err != nil {
			return "", fmt.Errorf("CodeAssistantOffline 对话轮 %d parts 无效", contentIndex)
		}
		for partIndex, partRaw := range parts {
			var part []json.RawMessage
			if err := json.Unmarshal(partRaw, &part); err != nil {
				return "", fmt.Errorf("CodeAssistantOffline 对话轮 %d part %d 无效", contentIndex, partIndex)
			}
			values = append(values, assistantBindingPart(part))
		}
	}
	return strings.Join(values, " "), nil
}

// assistantBindingPart returns the binding contribution of one encoded Part:
// text (field 2), inline data base64 (field 3) or Drive file ID (field 6).
func assistantBindingPart(part []json.RawMessage) string {
	if text := rawAt(part, 1); !isJSONNull(text) {
		if value, err := rawString(text, "$part[1]", text); err == nil {
			return value
		}
	}
	if inline := rawAt(part, 2); !isJSONNull(inline) {
		var data []json.RawMessage
		if json.Unmarshal(inline, &data) == nil && len(data) > 1 {
			if value, err := rawString(data[1], "$part[2][1]", inline); err == nil {
				return value
			}
		}
	}
	if file := rawAt(part, 5); !isJSONNull(file) {
		var data []json.RawMessage
		if json.Unmarshal(file, &data) == nil && len(data) > 0 {
			if value, err := rawString(data[0], "$part[5][0]", file); err == nil {
				return value
			}
		}
	}
	return ""
}

// assistantRequestID returns the uppercase UUID v4 (8-4-4-4-12, hyphenated) the
// assistant request carries in field 29, matching upstream
// strings.ToUpper(uuid.NewString()).
func assistantRequestID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("生成 CodeAssistantOffline 请求 ID: %w", err)
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	encoded := strings.ToUpper(hex.EncodeToString(raw[:]))
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

// assistantCleanup cancels an unfinished task and deletes the temporary applet.
// It never touches resources this request did not create.
type assistantCleanup struct {
	client     *Client
	ctx        context.Context
	accountID  string
	applet     string
	generation string
	completed  atomic.Bool
	once       sync.Once
}

// run executes the cleanup exactly once, synchronously, with its own deadline.
// It keeps the original request context values (notably the same account lease)
// through context.WithoutCancel so cleanup runs under the request's own lease
// instead of re-acquiring or borrowing another account. Cancel runs only when a
// task ID exists and the stream did not decode a completed reply; DeleteApplet
// always runs for the applet this request created.
func (cleanup *assistantCleanup) run() {
	if cleanup == nil {
		return
	}
	cleanup.once.Do(func() {
		ctx, cancel := context.WithTimeout(cleanup.baseContext(), assistantCleanupTimeout)
		defer cancel()
		if cleanup.generation != "" && !cleanup.completed.Load() {
			cleanup.call(ctx, assistantCancelMethod, []any{cleanup.generation, cleanup.applet})
		}
		cleanup.call(ctx, assistantDeleteAppletMethod, []any{cleanup.applet})
	})
}

// baseContext returns the cleanup context: the original request values without
// its cancellation, so a canceled client still cleans up under the same lease.
func (cleanup *assistantCleanup) baseContext() context.Context {
	if cleanup.ctx == nil {
		return context.Background()
	}
	return cleanup.ctx
}

func (cleanup *assistantCleanup) call(ctx context.Context, method string, value []any) {
	body, err := json.Marshal(value)
	if err == nil {
		var response *RPCResponse
		response, err = cleanup.client.do(ctx, method, cleanup.accountID, "", body, false)
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			err = errors.Join(err, response.Body.Close())
		}
	}
	if err != nil {
		slog.Warn("Build 写代码助手清理失败", "account", cleanup.accountID, "method", method, "applet", cleanup.applet, "error", err)
	}
}

// assistantStreamBody triggers cleanup when the task stream is closed. The decode
// closure also runs cleanup synchronously once decoding finishes, so a completed
// decode marks the task done before the body close schedules Cancel/Delete; the
// sync.Once makes the second trigger a no-op.
type assistantStreamBody struct {
	io.ReadCloser
	cleanup *assistantCleanup
}

// Close closes the task stream and runs cleanup.
func (body *assistantStreamBody) Close() error {
	err := body.ReadCloser.Close()
	body.cleanup.run()
	return err
}

// createAssistantApplet creates the temporary applet that carries one request.
// This is an auxiliary RPC: it must not be registered as a generation attempt.
func (c *Client) createAssistantApplet(ctx context.Context, accountID string) (string, error) {
	body, err := json.Marshal([]any{nil, []any{"Untitled", "Generated by Gemini.", nil, nil, "default"}, nil, nil, int64(1)})
	if err != nil {
		return "", err
	}
	response, err := c.do(ctx, assistantCreateAppletMethod, accountID, "", body, false)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("读取 AI Studio %s: %w", assistantCreateAppletMethod, err)
	}
	values, err := rawArray(raw, "$", raw)
	if err != nil {
		return "", withMethod(err, assistantCreateAppletMethod)
	}
	applet, err := rawString(rawAt(values, 0), "$[0]", raw)
	if err != nil || applet == "" {
		return "", &ProtocolEvidenceError{Method: assistantCreateAppletMethod, Path: "$[0]", Detail: "缺少应用 ID", Raw: cloneRaw(raw)}
	}
	return applet, nil
}

// decodeAssistantGeneration reads the task ID from the CodeAssistantOffline
// response [task ID, applet ID].
func decodeAssistantGeneration(source io.Reader) (string, error) {
	raw, err := io.ReadAll(source)
	if err != nil {
		return "", fmt.Errorf("读取 AI Studio %s: %w", assistantInitMethod, err)
	}
	values, err := rawArray(raw, "$", raw)
	if err != nil {
		return "", withMethod(err, assistantInitMethod)
	}
	generation, err := rawString(rawAt(values, 0), "$[0]", raw)
	if err != nil || generation == "" {
		return "", &ProtocolEvidenceError{Method: assistantInitMethod, Path: "$[0]", Detail: "缺少任务 ID", Raw: cloneRaw(raw)}
	}
	return generation, nil
}

// sendAssistant runs the two-stage Build code-assistant route. The
// CodeAssistantOffline submit is the WAA-protected generation send and is
// registered by the protected transport; CreateApplet, the step-snapshot stream,
// Cancel and DeleteApplet are auxiliary sends and are never registered as
// generation attempts. The returned response carries the submit's generation
// attempt so the shared loop observes usage and terminal events on it.
func (c *Client) sendAssistant(ctx context.Context, request GenerateRequest, entry modelEntry) (_ *RPCResponse, _ func(io.Reader, func(Event) error) error, resultErr error) {
	var err error
	request, err = prepareAssistantTextRequest(request)
	if err != nil {
		return nil, nil, err
	}
	if err := validateAssistantRequest(request); err != nil {
		return nil, nil, err
	}
	reason := ""
	if request.Unary {
		reason = "Build 写代码助手使用服务端流，收集完成后返回"
	}
	reportUpstreamMode(ctx, assistantInitMethod, "stream", reason)

	applet, err := c.createAssistantApplet(ctx, request.AccountID)
	if err != nil {
		return nil, nil, err
	}
	cleanup := &assistantCleanup{client: c, ctx: context.WithoutCancel(ctx), accountID: request.AccountID, applet: applet}

	requestID, err := assistantRequestID()
	if err != nil {
		cleanup.run()
		return nil, nil, err
	}
	body, err := EncodeAssistantRequest(request, entry.defaults, applet, requestID)
	if err != nil {
		cleanup.run()
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	rpc := newRPCRequest(assistantInitMethod, request.AccountID, request.ID, body, false)
	c.applyBenefitTier(rpc.Method, request.AccountID, rpc.Header)
	response, err := c.protected.DoProtected(ctx, request, rpc)
	var submitAttempt *physicalAttempt
	if response != nil {
		submitAttempt = response.attempt
	}
	// Setup/read-side errors belong to the original submit, not a new attempt.
	defer func() { submitAttempt.fail(resultErr) }()
	if err == nil && response != nil && response.StatusCode == http.StatusOK {
		// The upstream accepted the generation submit. Even when the task ID or
		// the step stream fails next, the submit may already have executed, so
		// conservatively forbid the outer no-content replay from submitting a
		// second generation for the same client request.
		observeGenerationProgress(ctx)
	}
	if err == nil {
		response, err = validateRPCResponse(assistantInitMethod, response)
	}
	if err != nil {
		cleanup.run()
		return nil, nil, fmt.Errorf("发送 AI Studio %s: %w", assistantInitMethod, err)
	}
	cleanup.generation, err = decodeAssistantGeneration(response.Body)
	response.Body.Close()
	if err != nil {
		cleanup.run()
		return nil, nil, err
	}

	streamBody, err := json.Marshal([]any{cleanup.generation, nil, nil, applet})
	if err != nil {
		cleanup.run()
		return nil, nil, err
	}
	stream, err := c.do(ctx, assistantStreamMethod, request.AccountID, request.ID, streamBody, true)
	if err != nil {
		cleanup.run()
		return nil, nil, err
	}
	// The task-snapshot stream is part of the same generation the protected
	// submit started; observe its events on that attempt instead of inventing a
	// second, differently-channelled attempt.
	stream.attempt = response.attempt
	stream.Body = &assistantStreamBody{ReadCloser: stream.Body, cleanup: cleanup}
	return stream, func(source io.Reader, emit func(Event) error) error {
		observed := func(event Event) error {
			stream.attempt.observe(event)
			reportRawGenerationEvent(ctx, event)
			return emit(event)
		}
		err := DecodeAssistantStream(source, &AssistantStreamDecoder{}, observed)
		if err == nil {
			cleanup.completed.Store(true)
		}
		stream.attempt.decoded(err, false)
		// Finish cleanup synchronously here, while the request still holds its own
		// lease, instead of racing the outer release in a detached goroutine.
		cleanup.run()
		return err
	}, nil
}

// AssistantStreamDecoder turns CodeAssistantOffline step snapshots into deltas.
// The assistant re-sends the full text snapshot each time, so only the new
// suffix is emitted.
type AssistantStreamDecoder struct {
	emitted    map[int64][]string
	usage      map[int64]Usage
	lastStep   int64
	wroteText  bool
	finished   bool
	stepStatus map[int64]int64
}

// DecodeAssistantStream decodes StreamCodeAssistantOfflineGeneration: the root
// array's field 1 is the message list, optionally followed by a google.rpc
// status. A stream that closes before a completed model reply is an error, never
// a synthesized success. Evidence: docs/build.md:610-660.
func DecodeAssistantStream(source io.Reader, decoder *AssistantStreamDecoder, emit func(Event) error) (resultErr error) {
	if decoder == nil {
		decoder = &AssistantStreamDecoder{}
	}
	// Preserve known completed-step usage even if a later step, envelope or
	// client projection fails. Repeated snapshots replace, never add, usage.
	defer func() {
		if resultErr != nil {
			if usage := decoder.usageSnapshot(); usage != nil {
				_ = emit(Event{Kind: EventUsage, Usage: usage})
			}
		}
	}()
	stream := json.NewDecoder(newSparseJSONReader(source))
	stream.UseNumber()
	index := 0
	fail := func(path string, detail string) error {
		return &ProtocolEvidenceError{Method: assistantStreamMethod, Path: path, Detail: detail}
	}
	if token, err := stream.Token(); err != nil || token != json.Delim('[') {
		return fail("$", "根值不是数组")
	}
	if !stream.More() {
		return fail("$", "根数组缺少消息列表")
	}
	if token, err := stream.Token(); err != nil || token != json.Delim('[') {
		return fail("$[0]", "消息列表不是数组")
	}
	for ; stream.More(); index++ {
		var raw json.RawMessage
		if err := stream.Decode(&raw); err != nil {
			return fail(fmt.Sprintf("$[0][%d]", index), err.Error())
		}
		events, err := decoder.Decode(raw, fmt.Sprintf("$[0][%d]", index))
		if err != nil {
			return err
		}
		for _, event := range events {
			if err := emit(event); err != nil {
				return err
			}
		}
	}
	if token, err := stream.Token(); err != nil || token != json.Delim(']') {
		return fail("$[0]", "消息列表没有正常结束")
	}
	for stream.More() {
		var raw json.RawMessage
		if err := stream.Decode(&raw); err != nil {
			return fail("$", err.Error())
		}
		if err := assistantTrailerError(raw); err != nil {
			return err
		}
	}
	if token, err := stream.Token(); err != nil || token != json.Delim(']') {
		return fail("$", "根数组没有正常结束")
	}
	// The closed root array is the in-band completion boundary. Do not probe
	// for transport EOF after it: the server may keep the HTTP stream open.
	// Root closure, trailer errors and End's completed-step checks still apply.
	events, err := decoder.End()
	if err != nil {
		return err
	}
	for _, event := range events {
		if err := emit(event); err != nil {
			return err
		}
	}
	return nil
}

// Decode decodes one message [null, null, [steps...], applet].
func (d *AssistantStreamDecoder) Decode(raw json.RawMessage, path string) ([]Event, error) {
	if d.emitted == nil {
		d.emitted = make(map[int64][]string)
		d.usage = make(map[int64]Usage)
		d.stepStatus = make(map[int64]int64)
	}
	message, err := rawArray(raw, path, raw)
	if err != nil {
		return nil, withMethod(err, assistantStreamMethod)
	}
	stepsRaw := rawAt(message, 2)
	if isJSONNull(stepsRaw) {
		return nil, nil
	}
	steps, err := rawArray(stepsRaw, path+"[2]", raw)
	if err != nil {
		return nil, withMethod(err, assistantStreamMethod)
	}
	var events []Event
	for stepIndex, stepRaw := range steps {
		stepEvents, err := d.decodeStep(stepRaw, fmt.Sprintf("%s[2][%d]", path, stepIndex), raw)
		if err != nil {
			return nil, err
		}
		events = append(events, stepEvents...)
	}
	return events, nil
}

func (d *AssistantStreamDecoder) decodeStep(raw json.RawMessage, path string, evidence json.RawMessage) ([]Event, error) {
	step, err := rawArray(raw, path, evidence)
	if err != nil {
		return nil, withMethod(err, assistantStreamMethod)
	}
	number, err := rawInt64(rawAt(step, 11), path+"[11]", evidence)
	if err != nil {
		return nil, withMethod(err, assistantStreamMethod)
	}
	status, err := rawInt64(rawAt(step, 12), path+"[12]", evidence)
	if err != nil {
		return nil, withMethod(err, assistantStreamMethod)
	}
	if failure := rawAt(step, 5); !isJSONNull(failure) {
		values, err := rawArray(failure, path+"[5]", evidence)
		if err != nil {
			return nil, withMethod(err, assistantStreamMethod)
		}
		message, err := rawString(rawAt(values, 0), path+"[5][0]", evidence)
		if err != nil {
			return nil, withMethod(err, assistantStreamMethod)
		}
		return nil, &RPCError{Method: assistantStreamMethod, StatusCode: http.StatusBadGateway, Message: "生成步骤失败: " + message}
	}
	switch status {
	case 1, 2:
	case 4:
		return nil, &RPCError{Method: assistantStreamMethod, StatusCode: http.StatusBadGateway, Message: "生成步骤失败（上游未提供详情）"}
	default:
		return nil, &ProtocolEvidenceError{Method: assistantStreamMethod, Path: path + "[12]", Detail: "未知的步骤状态"}
	}
	d.stepStatus[number] = status
	var events []Event
	if usageRaw := rawAt(step, 15); !isJSONNull(usageRaw) && status == 1 {
		usage, err := decodeAssistantUsage(usageRaw, path+"[15]", evidence)
		if err != nil {
			return nil, err
		}
		d.usage[number] = usage
		// Usage from this very snapshot precedes text: local stop must not
		// replace an already observed upstream count with an estimate.
		events = append(events, Event{Kind: EventUsage, Usage: d.usageSnapshot()})
	}
	contentsRaw := rawAt(step, 0)
	if isJSONNull(contentsRaw) {
		return events, nil
	}
	contents, err := rawArray(contentsRaw, path+"[0]", evidence)
	if err != nil {
		return nil, withMethod(err, assistantStreamMethod)
	}
	partOffset := 0
	for contentIndex, contentRaw := range contents {
		contentPath := fmt.Sprintf("%s[0][%d]", path, contentIndex)
		content, err := rawArray(contentRaw, contentPath, evidence)
		if err != nil {
			return nil, withMethod(err, assistantStreamMethod)
		}
		role, err := rawString(rawAt(content, 1), contentPath+"[1]", evidence)
		if err != nil {
			return nil, withMethod(err, assistantStreamMethod)
		}
		if role != "model" {
			continue
		}
		parts, err := rawArray(rawAt(content, 0), contentPath+"[0]", evidence)
		if err != nil {
			return nil, withMethod(err, assistantStreamMethod)
		}
		for partIndex, partRaw := range parts {
			partEvents, err := d.decodePart(number, partOffset+partIndex, partRaw, fmt.Sprintf("%s[0][%d]", contentPath, partIndex), evidence)
			if err != nil {
				return nil, err
			}
			events = append(events, partEvents...)
		}
		partOffset += len(parts)
		if status == 1 {
			d.finished = true
		}
	}
	return events, nil
}

// decodePart returns the text appended since the previous snapshot for one model
// Part. A snapshot that does not extend the previous one is a protocol error.
func (d *AssistantStreamDecoder) decodePart(step int64, index int, raw json.RawMessage, path string, evidence json.RawMessage) ([]Event, error) {
	part, err := rawArray(raw, path, evidence)
	if err != nil {
		return nil, withMethod(err, assistantStreamMethod)
	}
	if label, ok := assistantResponseToolSlot(part); ok {
		return nil, assistantResponseToolEvidenceError(path, label)
	}
	// Never silently turn unsupported native tool/media payloads into an empty
	// text success. These shared Part slots have no Assistant projection yet.
	for _, slot := range []int{2, 5, 7, 8, 10, 11} {
		if !isJSONNull(rawAt(part, slot)) {
			return nil, &ProtocolEvidenceError{Method: assistantStreamMethod, Path: fmt.Sprintf("%s[%d]", path, slot), Detail: "assistant output Part mapping not implemented"}
		}
	}
	textRaw := rawAt(part, 1)
	if isJSONNull(textRaw) {
		return nil, nil
	}
	text, err := rawString(textRaw, path+"[1]", evidence)
	if err != nil {
		return nil, withMethod(err, assistantStreamMethod)
	}
	thought := false
	if thoughtRaw := rawAt(part, 12); !isJSONNull(thoughtRaw) {
		thought, err = rawBool(thoughtRaw, path+"[12]", evidence)
		if err != nil {
			return nil, withMethod(err, assistantStreamMethod)
		}
	}
	emitted := d.emitted[step]
	for len(emitted) <= index {
		emitted = append(emitted, "")
	}
	previous := emitted[index]
	if !strings.HasPrefix(text, previous) {
		return nil, &ProtocolEvidenceError{Method: assistantStreamMethod, Path: path + "[1]", Detail: "步骤正文不是上一次快照的延续", Raw: cloneRaw(raw)}
	}
	emitted[index] = text
	d.emitted[step] = emitted
	delta := text[len(previous):]
	if delta == "" {
		return nil, nil
	}
	if thought {
		return []Event{{Kind: EventReasoning, Text: delta}}, nil
	}
	if d.wroteText && previous == "" && step != d.lastStep {
		delta = "\n\n" + delta
	}
	d.wroteText = true
	d.lastStep = step
	return []Event{{Kind: EventText, Text: delta}}, nil
}

// End emits the summed usage and the stop terminal event. It refuses to report
// success when no completed model reply was observed.
func (d *AssistantStreamDecoder) End() ([]Event, error) {
	if !d.finished {
		return nil, &ProtocolEvidenceError{Method: assistantStreamMethod, Path: "$", Detail: "流结束前没有完成的模型回复"}
	}
	for _, status := range d.stepStatus {
		if status != 1 {
			return nil, &ProtocolEvidenceError{Method: assistantStreamMethod, Path: "$", Detail: "流结束前仍有未完成步骤"}
		}
	}
	events := make([]Event, 0, 2)
	if usage := d.usageSnapshot(); usage != nil {
		events = append(events, Event{Kind: EventUsage, Usage: usage})
	}
	return append(events, Event{Kind: EventFinish, FinishReason: decodeFinishReason(1)}), nil
}

func (d *AssistantStreamDecoder) usageSnapshot() *Usage {
	if len(d.usage) == 0 {
		return nil
	}
	total := &Usage{Source: "upstream"}
	for _, usage := range d.usage {
		total.InputTokens += usage.InputTokens
		total.CachedTokens += usage.CachedTokens
		total.OutputTokens += usage.OutputTokens
		total.TotalTokens += usage.TotalTokens
	}
	return total
}

// decodeAssistantUsage decodes a step usage [uncached input, output, cached].
func decodeAssistantUsage(raw json.RawMessage, path string, evidence json.RawMessage) (Usage, error) {
	values, err := rawArray(raw, path, evidence)
	if err != nil {
		return Usage{}, withMethod(err, assistantStreamMethod)
	}
	numbers := make([]int64, 3)
	for index := range numbers {
		if value := rawAt(values, index); !isJSONNull(value) {
			numbers[index], err = rawInt64(value, fmt.Sprintf("%s[%d]", path, index), evidence)
			if err != nil {
				return Usage{}, withMethod(err, assistantStreamMethod)
			}
		}
	}
	input := numbers[0] + numbers[2]
	return Usage{InputTokens: input, CachedTokens: numbers[2], OutputTokens: numbers[1], TotalTokens: input + numbers[1]}, nil
}

// assistantTrailerError decodes the google.rpc status after the message list and
// reports it under the assistant stream method.
func assistantTrailerError(raw json.RawMessage) error {
	err := buildTrailerError(raw)
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		rpcErr.Method = assistantStreamMethod
	}
	var evidence *ProtocolEvidenceError
	if errors.As(err, &evidence) {
		evidence.Method = assistantStreamMethod
	}
	return err
}
