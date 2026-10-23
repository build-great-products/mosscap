package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/doctor"
	"github.com/stayradiated/mosscap/internal/identity"
	"github.com/stayradiated/mosscap/internal/protocol"
	"github.com/stayradiated/mosscap/internal/server"
	"github.com/stayradiated/mosscap/internal/slackruntime"
)

// The typed methods below are the Cobra entry points for setup, config, and
// gateway leaves.
func (a App) setupTyped(jsonOut bool) error {
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	c, err := config.Setup(loc)
	if err != nil {
		return err
	}
	return a.render(config.Redact(c), jsonOut)
}

func (a App) configShowTyped(jsonOut bool) error {
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	c, err := config.Load(loc)
	if err != nil {
		return err
	}
	return a.render(config.Redact(c), jsonOut)
}

func (a App) configSetTyped(key, value string, jsonOut bool) error {
	if key == "slack_app_token" || key == "slack_bot_token" {
		return usage("Slack credentials must use `mosscap config secret set " + key + "`")
	}
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	c, err := config.Set(loc, key, value)
	if err != nil {
		return err
	}
	return a.render(config.Redact(c), jsonOut)
}

func (a App) configSecretSetTyped(key string, stdin bool) error {
	if key != "slack_app_token" && key != "slack_bot_token" {
		return usage("config secret set requires slack_app_token or slack_bot_token")
	}
	value, err := a.readSecret(stdin)
	if err != nil {
		return err
	}
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	if _, err := config.SetSecret(loc, key, value); err != nil {
		return err
	}
	fmt.Fprintln(a.Out, key+" updated")
	return nil
}

func (a App) gatewayRunTyped() (err error) {
	// Gateway startup has passed Cobra parsing. Preserve startup failures as
	// runtime errors rather than allowing the command tree to render them as
	// usage errors (and obscure their operator guidance).
	defer func() {
		if err == nil {
			return
		}
		var command *commandError
		if !errors.As(err, &command) {
			err = &commandError{code: ExitUnsafe, message: err.Error()}
		}
	}()

	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	c, err := config.Load(loc)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &commandError{code: ExitUnsafe, message: "configuration is missing; run mosscap setup"}
		}
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return server.NewWithOptions(c, loc, a.Version, slog.New(slog.NewTextHandler(a.Err, nil)), server.Options{Factory: slackruntime.RealFactory{}}).Serve(ctx)
}

func (a App) gatewayReloadTyped(requestID string, jsonOut bool) error {
	if requestID == "" {
		id, err := identity.NewGenerator(nil).NewRequestID()
		if err != nil {
			return err
		}
		requestID = id
	}
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	c, err := config.Default(loc)
	if err != nil {
		return err
	}
	result, err := a.gatewayCall(c, "config.reload", requestID, nil, protocol.CapabilityConfigReload)
	if err != nil {
		return mutationJSONError(err, true, jsonOut, requestID)
	}
	return a.render(map[string]any{"request_id": requestID, "result": result}, jsonOut)
}

func (a App) gatewayStatusTyped(jsonOut bool) error {
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	c, err := config.Default(loc)
	if err != nil {
		return err
	}
	result, err := a.gatewayCall(c, "status.get", "req_status", nil, "")
	if err != nil {
		return err
	}
	return a.render(result, jsonOut)
}

func (a App) doctorTyped(jsonOut bool) error {
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	c, configErr := config.Load(loc)
	if configErr != nil {
		c, err = config.Default(loc)
		if err != nil {
			return err
		}
	}
	checks := localDoctorChecks(loc, c, configErr)
	remoteResult, remoteErr := a.gatewayCall(c, "doctor.run", "req_doctor", nil, "")
	if remoteErr != nil {
		checks = append(checks, doctor.Check{Name: "gateway_diagnostics", Status: doctor.Skipped, Message: "gateway diagnostics unavailable", Action: "start mosscap gateway run"})
		report := doctor.Report{Overall: doctor.Overall(checks), Checks: checks}
		if !jsonOut {
			if err := a.renderDoctor(report, false); err != nil {
				return err
			}
		}
		return &doctorPartialError{err: &commandError{code: ExitUnavailable, message: "gateway diagnostics unavailable"}, report: report}
	}
	b, _ := json.Marshal(remoteResult)
	var remote doctor.Report
	if err := json.Unmarshal(b, &remote); err != nil {
		return err
	}
	checks = append(checks, remote.Checks...)
	return a.renderDoctor(doctor.Report{Overall: doctor.Overall(checks), Checks: checks}, jsonOut)
}
