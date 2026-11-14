package aistudio

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// questionToolParameters 表示支持文字或对象选项的问题工具
const questionToolParameters = `{"type":"object","properties":{"questions":{"type":"array","items":{"type":"object","properties":{"multiSelect":{"type":"boolean"},"options":{"type":"array","items":{"anyOf":[{"type":"string"},{"type":"object","properties":{"description":{"type":"string"},"label":{"type":"string"}},"required":["label"]}]}},"question":{"type":"string"}},"required":["question","options"]}}},"required":["questions"]}`

// TestToolSchemaMixedOptions 验证复杂问题工具保持原始声明且直接使用 Playground
func TestToolSchemaMixedOptions(t *testing.T) {
	declaration := FunctionDeclaration{Name: "asktool", Parameters: json.RawMessage(questionToolParameters)}
	before, _ := json.Marshal(declaration)
	wire, err := encodeFunctionDeclaration(declaration)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(declaration)
	if string(before) != string(after) {
		t.Fatal("原始工具声明发生变化")
	}
	normalized, err := normalizeFunctionParameters(declaration.Parameters)
	if err != nil || !strings.Contains(wire[1].(string), string(normalized)) || schemaWireNeedsBuild(wire[2].([]any)) {
		t.Fatalf("完整约束或可发送的参数结构缺失: %#v", wire)
	}
	request := GenerateRequest{Tools: Tools{Functions: []FunctionDeclaration{declaration}}}
	if RequestNeedsBuildSchema(request) {
		t.Fatal("工具声明触发 Build 分流")
	}
	request.Config.ResponseSchema = json.RawMessage(`{"type":"object","properties":{"value":{}}}`)
	if !RequestNeedsBuildSchema(request) {
		t.Fatal("结构化输出的原生通道选择丢失")
	}
}

// TestToolSchemaUnionBranches 验证联合分支与首个数组元素类型保留
func TestToolSchemaUnionBranches(t *testing.T) {
	for _, test := range []struct {
		name, schema string
		index        int
	}{
		{"string object", `{"anyOf":[{"type":"string","minLength":2},{"type":"object","properties":{"label":{"type":"string"}},"required":["label"]}]}`, 17},
		{"object string", `{"anyOf":[{"type":"object","properties":{"label":{"type":"string"}},"required":["label"]},{"type":"string"}]}`, 17},
		{"overlapping oneOf", `{"oneOf":[{"type":"string"},{"type":"string","minLength":3},{"type":"object"}]}`, 16},
		{"array first", `{"anyOf":[{"type":"array","items":{"type":"integer"},"minItems":1},{"type":"string"}]}`, 17},
	} {
		t.Run(test.name, func(t *testing.T) {
			wire, err := encodeFunctionDeclaration(FunctionDeclaration{Name: "pick", Parameters: json.RawMessage(test.schema)})
			if err != nil {
				t.Fatal(err)
			}
			parameters := wire[2].([]any)
			var original map[string][]json.RawMessage
			_ = json.Unmarshal([]byte(test.schema), &original)
			key := "anyOf"
			if test.index == 16 {
				key = "oneOf"
			}
			actual := parameters[test.index].([]any)
			if len(actual) != len(original[key]) {
				t.Fatal("联合分支数量变化")
			}
			for index, branch := range original[key] {
				expected, err := encodeJSONSchema(branch)
				if err != nil || !reflect.DeepEqual(actual[index], expected) {
					t.Fatalf("联合分支 %d 变化: %#v, %v", index, actual[index], err)
				}
			}
			if test.name == "array first" && parameters[5].([]any)[0] != int64(3) {
				t.Fatal("数组元素类型变化")
			}
		})
	}
}

// TestToolSchemaOpenNodes 验证开放、引用与组合节点携带完整原始约束
func TestToolSchemaOpenNodes(t *testing.T) {
	for _, raw := range []string{
		`{"type":"object","properties":{"value":true}}`,
		`{"type":"array"}`,
		`{"anyOf":[{},{"type":"string"}]}`,
		`{"anyOf":[{"type":"array"},{"type":"string"}]}`,
		`{"anyOf":[{"type":"string"},{"type":"object"}],"not":{"type":"string"}}`,
		`{"type":"object","properties":{"value":{"const":7}}}`,
		`{"type":"object","properties":{"value":{"const":null}}}`,
		`{"type":"object","properties":{"value":{"$ref":"#/$defs/item"}},"$defs":{"item":{"type":"string"}}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			wire, err := encodeFunctionDeclaration(FunctionDeclaration{Name: "pick", Parameters: json.RawMessage(raw)})
			if err != nil {
				t.Fatal(err)
			}
			normalized, err := normalizeFunctionParameters(json.RawMessage(raw))
			if err != nil || !strings.Contains(wire[1].(string), string(normalized)) || schemaWireNeedsBuild(wire[2].([]any)) {
				t.Fatalf("约束或参数结构缺失: %#v", wire)
			}
		})
	}
}

// TestToolSchemaKeepsFileTools 验证问题工具与文件工具共同编码时保留普通声明
func TestToolSchemaKeepsFileTools(t *testing.T) {
	tools := Tools{Functions: []FunctionDeclaration{
		{Name: "Read", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)},
		{Name: "asktool", Parameters: json.RawMessage(questionToolParameters)},
		{Name: "Write", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`)},
	}}
	before, _ := json.Marshal(tools)
	wire, _, err := encodeRequestedTools(tools)
	if err != nil {
		t.Fatal(err)
	}
	declarations := wire[0].([]any)[1].([]any)
	for _, index := range []int{0, 2} {
		expected, err := encodeFunctionDeclaration(tools.Functions[index])
		if err != nil || !reflect.DeepEqual(declarations[index], expected) {
			t.Fatalf("文件工具 %d 变化", index)
		}
	}
	after, _ := json.Marshal(tools)
	if string(before) != string(after) {
		t.Fatal("原始工具集发生变化")
	}
}

// TestToolSchemaLongFunctionNames 验证声明、历史与计数使用同一短名，回传原名且不改调用方数据。
func TestToolSchemaLongFunctionNames(t *testing.T) {
	prefix := "mcp__codex_apps__codex_document_control___execute_document_"
	names := []string{prefix + "command", prefix + "commands", "Read", strings.Repeat("a", 63), strings.Repeat("b", 64)}
	functions := make([]FunctionDeclaration, 0, len(names))
	for _, name := range names {
		functions = append(functions, FunctionDeclaration{Name: name, Parameters: json.RawMessage(`{"type":"object"}`)})
	}
	before, _ := json.Marshal(functions)
	wire, _, err := encodeRequestedTools(Tools{Functions: functions})
	if err != nil {
		t.Fatal(err)
	}
	sent := make([]string, 0, len(names))
	for _, declaration := range wire[0].([]any)[1].([]any) {
		sent = append(sent, declaration.([]any)[0].(string))
	}
	for index, name := range sent {
		if len(name) > 63 || name != wireFunctionName(names[index]) {
			t.Fatalf("声明的函数名 %q", sent)
		}
	}
	if sent[0] == sent[1] || sent[2] != "Read" || sent[3] != names[3] || sent[4] == names[4] {
		t.Fatalf("边界或同前缀名称处理错误: %q", sent)
	}
	contents := []Content{
		{Role: RoleUser, Parts: []Part{{Text: "run"}}},
		{Role: RoleAssistant, Parts: []Part{{FunctionCall: &FunctionCall{ID: "call-1", Name: names[0], Arguments: json.RawMessage(`{}`)}}}},
		{Role: RoleUser, Parts: []Part{{FunctionResult: &FunctionResult{ID: "call-1", Content: json.RawMessage(`{"ok":true}`)}}}},
	}
	historyBefore, _ := json.Marshal(contents)
	encoded, err := encodeContents(contents)
	if err != nil {
		t.Fatal(err)
	}
	history, _ := json.Marshal(encoded)
	if strings.Count(string(history), `"`+sent[0]+`"`) != 2 || strings.Contains(string(history), names[0]) {
		t.Fatalf("历史中的函数名与声明不一致: %s", history)
	}
	count, err := EncodeCountTokensRequest(TokenCountRequest{Model: "fixture-model", Contents: contents, Tools: Tools{Functions: functions}})
	if err != nil || strings.Count(string(count), `"`+sent[0]+`"`) != 3 || strings.Contains(string(count), names[0]) {
		t.Fatalf("计数名称不一致: %s, %v", count, err)
	}
	var received []string
	emit := restoreFunctionNames(functions, func(event Event) error {
		received = append(received, event.ToolCall.Name)
		return nil
	})
	for _, name := range sent {
		call := &FunctionCall{Name: name}
		if err := emit(Event{Kind: EventToolCall, ToolCall: call}); err != nil {
			t.Fatal(err)
		}
		if call.Name != name {
			t.Fatal("回传改写了上游事件")
		}
	}
	if !reflect.DeepEqual(received, names) {
		t.Fatalf("回传的函数名 %q", received)
	}
	after, _ := json.Marshal(functions)
	historyAfter, _ := json.Marshal(contents)
	if string(before) != string(after) || string(historyBefore) != string(historyAfter) {
		t.Fatal("原始声明或历史发生变化")
	}
}

// TestLongFunctionNamePipeline 验证 PG 原名恢复先于本地工具契约，Build 保留原名。
func TestLongFunctionNamePipeline(t *testing.T) {
	name := "mcp__long_tool_" + strings.Repeat("action_", 10)
	for _, channel := range []Channel{ChannelPlayground, ChannelBuild} {
		for _, anti := range []bool{false, true} {
			model := string(channel) + "/"
			if anti {
				model += "anti-truncation/"
			}
			model += "fixture-model"
			t.Run(model, func(t *testing.T) {
				fixture := newPipelineFixture(t, channel)
				fixture.reply = func(request GenerateRequest, rpc RPCRequest, _ int) ([]byte, error) {
					wire := string(rpc.Body)
					want, unwanted := wireFunctionName(name), name
					if channel == ChannelBuild {
						want, unwanted = name, wireFunctionName(name)
					}
					if !strings.Contains(wire, want) || strings.Contains(wire, unwanted) && channel == ChannelBuild {
						t.Fatalf("通道工具名称错误: %s", wire)
					}
					parts := []Part{{FunctionCall: &FunctionCall{ID: "call-long", Name: want, Arguments: json.RawMessage(`{"value":"ok"}`)}, ThoughtSignature: "long-signature"}}
					return fixtureResponse(t, rpc, parts, "stop", &Usage{InputTokens: 10, OutputTokens: 9, TotalTokens: 19}, ""), nil
				}
				events := fixture.generate(t, GenerateRequest{Model: model, Tools: Tools{
					Functions:  []FunctionDeclaration{{Name: name, Parameters: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`), Strict: true}},
					ToolConfig: ToolConfig{Mode: "required", AllowedFunctionNames: []string{name}},
				}})
				if err := pipelineError(events); err != nil {
					t.Fatal(err)
				}
				calls := 0
				for _, event := range events {
					if event.ToolCall != nil {
						calls++
						if event.ToolCall.Name != name || event.ToolCall.ID != "call-long" || string(event.ToolCall.Arguments) != `{"value":"ok"}` || event.ThoughtSignature != "long-signature" {
							t.Fatalf("公开工具调用发生变化: %+v", event)
						}
					}
				}
				if calls != 1 || pipelineFinish(events) == nil || len(fixture.sends) != 1 {
					t.Fatalf("调用/终态/请求次数错误: %d/%+v/%d", calls, pipelineFinish(events), len(fixture.sends))
				}
			})
		}
	}
}

func TestToolSchemaWireNameCollision(t *testing.T) {
	longName := strings.Repeat("a", 64)
	shortName := wireFunctionName(longName)
	for _, names := range [][]string{{longName, shortName}, {shortName, longName}} {
		t.Run(names[0], func(t *testing.T) {
			tools := Tools{Functions: []FunctionDeclaration{{Name: names[0]}, {Name: names[1]}}}
			before, _ := json.Marshal(tools)
			contents := []Content{{Role: RoleUser, Parts: []Part{{Text: "run"}}}}
			fixture := newPipelineFixture(t, ChannelPlayground)
			fixture.reply = func(GenerateRequest, RPCRequest, int) ([]byte, error) {
				return nil, errors.New("unexpected physical send")
			}
			_, generateErr := fixture.client.Generate(fixture.ctx, GenerateRequest{
				ID: t.Name(), AccountID: fixture.account.ID, Model: "playground/fixture-model",
				Contents: contents, Tools: tools,
			})
			_, countErr := fixture.client.CountTokens(fixture.ctx, TokenCountRequest{
				Model: "fixture-model", Contents: contents, Tools: tools,
			})
			for _, err := range []error{generateErr, countErr} {
				if !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "函数名冲突") || !strings.Contains(err.Error(), longName) || !strings.Contains(err.Error(), shortName) {
					t.Fatalf("未清晰拒绝冲突的工具声明: %v", err)
				}
			}
			if len(fixture.sends) != 0 {
				t.Fatalf("冲突工具声明已发往上游: %d", len(fixture.sends))
			}
			wire, _, err := encodeBuildTools(tools)
			if err != nil {
				t.Fatal(err)
			}
			build, _ := json.Marshal(wire)
			if !strings.Contains(string(build), `"`+longName+`"`) || !strings.Contains(string(build), `"`+shortName+`"`) {
				t.Fatalf("Build 的不同原名被改写: %s", build)
			}
			after, _ := json.Marshal(tools)
			if string(before) != string(after) {
				t.Fatal("原始工具声明发生变化")
			}
		})
	}
}
