package authhelper

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// RunCLI is also embedded in the Linux generator as the auth-helper subcommand.
// Without both execution flags it prints a plan and does not read config/auth/keys.
func RunCLI(args []string, out, errOut io.Writer) int {
	options := flag.NewFlagSet("auth-helper", flag.ContinueOnError)
	options.SetOutput(io.Discard)
	action := options.String("operation", "plan", "")
	config := options.String("config", "", "")
	handle := options.String("handle", "", "")
	epoch := options.Uint64("expected-epoch", 0, "")
	targetPID := options.Int("target-pid", 0, "")
	execute := options.Bool("execute", false, "")
	confirm := options.Bool("confirm-account", false, "")
	sourceRecovery := options.Bool("allow-source-recovery", false, "")
	acknowledgePrior := options.Bool("acknowledge-prior-operations", false, "")
	oldConsumerStopped := options.Bool("confirm-old-consumer-stopped", false, "")
	keepCurrent := options.Bool("keep-current-cookies", false, "")
	chromeRoot := options.String("chrome-root", "", "")
	profile := options.String("profile", "", "")
	statePath := options.String("state", "", "")
	authRoot := options.String("auth-root", "", "")
	email := options.String("email", "", "")
	gaia := options.String("gaia-id", "", "")
	baseCookie := options.Uint64("base-cookie", 0, "")
	baseMaterial := options.Uint64("base-material", 1, "")
	if options.Parse(args) != nil || options.NArg() != 0 {
		writeCLI(errOut, map[string]any{"code": "INVALID_REQUEST"})
		return 2
	}
	allowed := map[string]bool{"plan": true, "serve": true, "register": true, "enable": true, "disable": true, "inspect": true, "bootstrap": true, "reconcile": true, "rebind-source": true, "reenroll": true, "health": true, "signal-stop": true, "return-local": true}
	if !allowed[*action] {
		writeCLI(errOut, map[string]any{"code": "INVALID_REQUEST"})
		return 2
	}
	if !*execute || !*confirm || *action == "plan" {
		writeCLI(out, map[string]any{"operation": *action, "execute": false, "account_confirmed": false, "remote_default": "disabled", "config_read": false, "auth_read": false})
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch *action {
	case "signal-stop":
		err = signalLocalStop(*targetPID)
	case "health":
		var settings ClientSettings
		settings, err = ReadClientSettings(*config)
		if err == nil {
			var client *Client
			client, err = LoadClient(settings)
			if err == nil {
				defer client.Close()
				err = client.Health(ctx)
			}
		}
	case "serve":
		err = serveNative(ctx, *config)
	case "register", "enable", "disable", "rebind-source", "return-local":
		var settings ServerSettings
		settings, err = ReadServerSettings(*config)
		if err == nil {
			_, err = NewBroker(ctx, settings.Mappings, NativeRenew)
			var selected *Mapping
			for i := range settings.Mappings {
				if settings.Mappings[i].Handle == *handle {
					selected = &settings.Mappings[i]
				}
			}
			if selected == nil {
				err = failure("IDENTITY_MISMATCH")
			}
			if err == nil {
				if *action == "register" {
					err = RegisterAuthority(ctx, *selected)
				} else if *action == "rebind-source" {
					source := aistudio.AuthSource{Browser: "chrome", ChromeRoot: *chromeRoot, Profile: *profile, GaiaID: selected.GaiaID, Email: selected.Email}
					err = RebindAuthoritySource(ctx, *selected, *epoch, source, *acknowledgePrior)
				} else if *action == "return-local" {
					err = ReturnLocalAuthority(ctx, *selected, *epoch, *oldConsumerStopped)
				} else {
					if _, e := WindowsIdentity(); e != nil {
						err = e
					} else {
						err = SetAuthorityPolicy(ctx, *selected, *epoch, *action == "enable", *sourceRecovery)
					}
				}
			}
		}
	case "reenroll":
		var settings ClientSettings
		settings, err = ReadClientSettings(*config)
		if err == nil {
			var client *Client
			client, err = LoadClient(settings)
			if err == nil {
				defer client.Close()
				binding := aistudio.RemoteSession{Handle: *handle, Consumer: settings.Consumer, GaiaID: *gaia, Email: *email, Epoch: *epoch, CookieRevision: *baseCookie, MaterialVersion: *baseMaterial}
				err = ReenrollNative(ctx, client, *statePath, binding, *acknowledgePrior)
			}
		}
	case "inspect":
		if !filepath.IsAbs(*statePath) {
			err = failure("INVALID_REQUEST")
			break
		}
		var state aistudio.StorageState
		state, err = aistudio.LoadStorageState(*statePath)
		if err == nil {
			var meta *aistudio.SessionState
			meta, err = state.Session()
			if err == nil && meta != nil {
				if a := meta.Authority; a != nil {
					stage := "none"
					if a.Operation != nil {
						stage = a.Operation.Stage
					}
					writeCLI(out, map[string]any{"role": "authority", "epoch": a.Epoch, "cookie_revision": a.CookieRevision, "material_version": a.MaterialVersion, "enabled": a.Enabled, "operation_stage": stage})
				} else {
					r := meta.Remote
					writeCLI(out, map[string]any{"role": "consumer", "epoch": r.Epoch, "cookie_revision": r.CookieRevision, "material_version": r.MaterialVersion, "pending": r.Pending != nil, "ack_pending": r.Ack != nil})
				}
				return 0
			}
			if err == nil {
				err = failure("MAPPING_DISABLED")
			}
		}
	case "bootstrap":
		binding := aistudio.RemoteSession{Handle: *handle, GaiaID: *gaia, Email: *email, Epoch: *epoch, CookieRevision: *baseCookie, MaterialVersion: *baseMaterial}
		err = bootstrapNative(ctx, *config, *authRoot, binding)
	case "reconcile":
		err = reconcileNative(ctx, *config, *statePath, *keepCurrent)
	}
	if err != nil {
		writeCLI(errOut, map[string]any{"code": safeCode(err), "operation": *action})
		return 1
	}
	writeCLI(out, map[string]any{"code": "OK", "operation": *action})
	return 0
}
func writeCLI(w io.Writer, value any) { _ = json.NewEncoder(w).Encode(value) }

func serveNative(ctx context.Context, path string) error {
	if _, err := WindowsIdentity(); err != nil {
		return err
	}
	settings, err := ReadServerSettings(path)
	if err != nil {
		return err
	}
	if !settings.Enabled || !LoopbackAddress(settings.Listen) {
		return failure("MAPPING_DISABLED")
	}
	ctx, stopLocal, err := localStopContext(ctx)
	if err != nil {
		return err
	}
	defer stopLocal()
	broker, err := NewBroker(ctx, settings.Mappings, NativeRenew)
	if err != nil {
		return err
	}
	config, err := ServerTLS(settings.TLS)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", settings.Listen)
	if err != nil {
		return failure("STATE_UNAVAILABLE")
	}
	defer listener.Close()
	server := &http.Server{Handler: Handler(broker), TLSConfig: config, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 150 * time.Second, IdleTimeout: 30 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	done := make(chan struct{})
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		select {
		case <-ctx.Done():
			// Stop accepting new work; do not drop a CNG lease on an arbitrary timer.
			_ = server.Shutdown(context.Background())
		case <-done:
		}
	}()
	err = server.ServeTLS(listener, "", "")
	close(done)
	<-shutdownDone
	if err != nil && err != http.ErrServerClosed {
		return failure("HELPER_UNAVAILABLE")
	}
	return nil
}

func bootstrapNative(ctx context.Context, config, root string, binding aistudio.RemoteSession) error {
	settings, err := ReadClientSettings(config)
	if err != nil {
		return err
	}
	client, err := LoadClient(settings)
	if err != nil {
		return err
	}
	defer client.Close()
	binding.Consumer = settings.Consumer
	meta := &aistudio.SessionState{Version: Protocol, Remote: &binding}
	state := aistudio.StorageState{}
	if state.SetSession(meta) != nil || !filepath.IsAbs(root) {
		return failure("INVALID_REQUEST")
	}
	store := aistudio.NewAccountStore(root)
	account, publish, err := store.CreateRemoteProjection(aistudio.DefaultAccountConfig(binding.Email), state)
	if err != nil {
		return failure("STATE_UNAVAILABLE")
	}
	defer publish.Release()
	state, err = aistudio.LoadStorageState(account.StoragePath)
	if err != nil {
		return failure("STATE_UNAVAILABLE")
	}
	if err = client.PrepareRecovery(ctx, &state, account.StoragePath, "bootstrap", nil); err != nil {
		return err
	}
	return commitNativeProjection(ctx, client, &state, account.StoragePath)
}

func reconcileNative(ctx context.Context, config, path string, keepCurrent ...bool) error {
	if !filepath.IsAbs(path) {
		return failure("INVALID_REQUEST")
	}
	settings, err := ReadClientSettings(config)
	if err != nil {
		return err
	}
	client, err := LoadClient(settings)
	if err != nil {
		return err
	}
	defer client.Close()
	return reconcileProjection(ctx, client, path, keepCurrent...)
}

func reconcileProjection(ctx context.Context, client *Client, path string, keepCurrent ...bool) error {
	release, err := aistudio.AcquireSessionAccountLease(ctx, path)
	if err != nil {
		return err
	}
	defer release()
	state, err := aistudio.LoadStorageState(path)
	if err != nil {
		return failure("STATE_UNAVAILABLE")
	}
	meta, err := state.Session()
	if err != nil || meta == nil || meta.Remote == nil {
		return failure("IDENTITY_MISMATCH")
	}
	if len(keepCurrent) > 0 && keepCurrent[0] {
		if err := client.ReconcileKeepingCurrent(ctx, &state, path); err != nil {
			return err
		}
		return commitNativeProjection(ctx, client, &state, path)
	}
	if meta.Remote.Pending != nil || meta.Remote.EnrollmentRequired || meta.Remote.RecoveryRequired {
		reason := "reauth"
		if meta.Remote.Pending != nil {
			reason = meta.Remote.Pending.Reason
		} else if meta.Remote.EnrollmentRequired {
			reason = "bootstrap"
		}
		ctx = aistudio.WithRemoteAuthBudget(ctx)
		if err = client.PrepareRecovery(ctx, &state, path, reason, func(id string) error { return aistudio.TakeRemoteAuthBudget(ctx, id) }); err != nil {
			return err
		}
		return commitNativeProjection(ctx, client, &state, path)
	}
	if meta.Remote.Ack == nil {
		return nil
	}
	return ackNativeProjection(ctx, client, &state, path)
}
func commitNativeProjection(ctx context.Context, client *Client, state *aistudio.StorageState, path string) error {
	if _, err := aistudio.NewSigner().Sign(*state); err != nil {
		return failure("COOKIE_POLICY_FAILURE")
	}
	if aistudio.WriteManagedSessionState(ctx, path, state) != nil {
		return failure("STATE_UNAVAILABLE")
	}
	return ackNativeProjection(ctx, client, state, path)
}
func ackNativeProjection(ctx context.Context, client *Client, state *aistudio.StorageState, path string) error {
	meta, err := state.Session()
	if err != nil || meta == nil || meta.Remote == nil {
		return failure("INVALID_REQUEST")
	}
	if meta.Remote.Ack == nil {
		return nil
	}
	if err = client.AckSession(ctx, meta.Remote); err != nil {
		return err
	}
	meta.Remote.Ack = nil
	if state.SetSession(meta) != nil || aistudio.WriteManagedSessionState(ctx, path, state) != nil {
		return failure("STATE_UNAVAILABLE")
	}
	return nil
}
