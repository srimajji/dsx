package app

import (
	"context"
	"fmt"

	"github.com/srimajji/dsx/internal/config"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
)

// RuntimeProber is the read-only runtime capability boundary used by doctor.
type RuntimeProber interface {
	Probe(context.Context) (runtime.Capabilities, error)
}

type DoctorService struct {
	prober RuntimeProber
}

func NewDoctorService(prober RuntimeProber) *DoctorService {
	return &DoctorService{prober: prober}
}

func (service *DoctorService) Doctor(ctx context.Context, request DoctorRequest) (DoctorResult, error) {
	if ctx == nil {
		return DoctorResult{}, model.NewError(model.CodeInvalidInput, "doctor: context is nil", nil)
	}
	if service == nil || service.prober == nil {
		return DoctorResult{}, model.NewError(model.CodeUnavailable, "Apple container runtime probe is unavailable; install container and ensure it is on PATH", nil)
	}
	capabilities, err := service.prober.Probe(ctx)
	result := DoctorResult{Capabilities: capabilities, Diagnostics: make([]config.Diagnostic, 0)}
	if err != nil {
		result.Diagnostics = append(result.Diagnostics, config.Diagnostic{
			Severity: "error",
			Code:     "runtime_unavailable",
			Message:  err.Error(),
		})
		if model.ErrorCodeOf(err) == model.CodeInvalidInput {
			return result, err
		}
		return result, model.Wrap(model.CodeUnavailable, "doctor", err)
	}
	if !capabilities.ServiceHealthy {
		result.Diagnostics = append(result.Diagnostics, config.Diagnostic{Severity: "error", Code: "service_unhealthy", Message: "Apple container service is not healthy; start it with `container system start` and retry"})
		return result, model.NewError(model.CodeUnavailable, result.Diagnostics[0].Message, nil)
	}
	if request.RequireBuilder && !capabilities.BuilderHealthy {
		result.Diagnostics = append(result.Diagnostics, config.Diagnostic{Severity: "error", Code: "builder_unavailable", Message: "Apple container builder is not healthy; run `container builder start` and retry"})
		return result, model.NewError(model.CodeUnavailable, result.Diagnostics[0].Message, nil)
	}
	if !capabilities.BuilderHealthy {
		result.Diagnostics = append(result.Diagnostics, config.Diagnostic{Severity: "warning", Code: "builder_gated", Message: "Apple container builder is not healthy; image builds are gated until `container builder start` succeeds"})
	}
	if capabilities.CompatibilityID == "" {
		return result, model.NewError(model.CodeUnavailable, fmt.Sprintf("runtime compatibility could not be established for container CLI %q", capabilities.CLIVersion), nil)
	}
	return result, nil
}
