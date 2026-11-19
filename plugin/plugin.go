package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// providerKey is the identifier this plugin registers with the host. It becomes
// the provider key of the executor and of the static model set.
const providerKey = "go-adapter"

// cfg is the live plugin configuration, refreshed on every register/reconfigure.

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

type capabilities struct {
	AuthProvider          bool     `json:"auth_provider"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope,omitempty"`
	ExecutorInputFormats  []string `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string `json:"executor_output_formats,omitempty"`
	ModelProvider         bool     `json:"model_provider"`
	Scheduler             bool     `json:"scheduler"`
	QuotaProvider         bool     `json:"quota_provider"`
	ManagementAPI         bool     `json:"management_api"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type modelInfo struct {
	ID          string `json:"ID"`
	Object      string `json:"Object"`
	OwnedBy     string `json:"OwnedBy"`
	Type        string `json:"Type"`
	DisplayName string `json:"DisplayName"`
	Name        string `json:"Name"`
}

type modelResponse struct {
	Provider string      `json:"Provider"`
	Models   []modelInfo `json:"Models"`
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

// envelopeError mirrors pluginabi.Error so the host reads the status, the retry
// hint and the credential scope out of every plugin failure.
type envelopeError struct {
	Code              string `json:"code"`
	Message           string `json:"message"`
	Retryable         bool   `json:"retryable,omitempty"`
	HTTPStatus        int    `json:"http_status,omitempty"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
	CredentialScoped  bool   `json:"credential_scoped,omitempty"`
}

func okEnvelope(v any) []byte {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return errorEnvelope("marshal_error", errMarshal.Error())
	}
	return marshalEnvelope(envelope{OK: true, Result: raw})
}

func (e *envelopeError) Error() string { return e.Message }

func errorEnvelope(code, message string) []byte {
	return marshalEnvelope(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
}

// errorResult keeps a structured error's status, retry hint and scope on the
// wire instead of flattening it into a bare code+message envelope.
func errorResult(err error) []byte {
	var structured *envelopeError
	if errors.As(err, &structured) {
		return marshalEnvelope(envelope{OK: false, Error: structured})
	}
	return errorEnvelope("executor_error", err.Error())
}

func marshalEnvelope(e envelope) []byte {
	raw, errMarshal := json.Marshal(e)
	if errMarshal != nil {
		return []byte(`{"ok":false,"error":{"code":"marshal_error","message":"envelope"}}`)
	}
	return raw
}

// handleMethod dispatches one RPC from the host.
func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var lr lifecycleRequest
		var decodeErr error
		if len(request) > 0 {
			decodeErr = json.Unmarshal(request, &lr)
		}
		parsed, errParse := parseConfig(lr.ConfigYAML)
		if errParse == nil && decodeErr != nil {
			errParse = decodeErr
		}
		if errParse != nil {
			// A broken config must never silently redirect traffic to the
			// default upstream: keep the last known-good configuration and
			// say so in the host log.
			if old := configPtr.Load(); old != nil {
				parsed = *old
			}
			_ = callHost(pluginabi.MethodHostLog, map[string]any{
				"level":   "warn",
				"message": "go-adapter config parse failed; keeping last known-good config",
				"fields":  map[string]any{"plugin": providerKey, "error": errParse.Error()},
			}, nil)
		}
		configPtr.Store(&parsed)
		return okEnvelope(pluginRegistration()), nil

	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(json.RawMessage(handleAuthIdentifier())), nil

	case pluginabi.MethodAuthParse:
		raw, err := handleAuthParse(request)
		if err != nil {
			return errorEnvelope("auth_parse_error", err.Error()), nil
		}
		return okEnvelope(json.RawMessage(raw)), nil

	case pluginabi.MethodQuotaIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerKey}), nil

	case pluginabi.MethodQuotaDescribe:
		raw, err := handleQuotaDescribe()
		if err != nil {
			return errorEnvelope("quota_error", err.Error()), nil
		}
		return okEnvelope(json.RawMessage(raw)), nil

	case pluginabi.MethodQuotaFetch:
		raw, err := handleQuotaFetch(request)
		if err != nil {
			return errorEnvelope("quota_error", err.Error()), nil
		}
		return okEnvelope(json.RawMessage(raw)), nil

	case pluginabi.MethodQuotaReset:
		return errorEnvelope("quota_reset_unsupported", "OpenCode Go quota cannot be reset from here"), nil

	case pluginabi.MethodManagementRegister:
		raw, err := handleManagementRegister()
		if err != nil {
			return errorEnvelope("management_error", err.Error()), nil
		}
		return okEnvelope(json.RawMessage(raw)), nil

	case pluginabi.MethodManagementHandle:
		raw, err := handleManagement(request)
		if err != nil {
			return errorEnvelope("management_error", err.Error()), nil
		}
		return okEnvelope(json.RawMessage(raw)), nil

	case pluginabi.MethodSchedulerPick:
		raw, err := handleSchedulerPick(request)
		if err != nil {
			return errorEnvelope("scheduler_error", err.Error()), nil
		}
		return okEnvelope(json.RawMessage(raw)), nil

	case pluginabi.MethodAuthRefresh:
		raw, err := handleAuthRefresh(request)
		if err != nil {
			return errorEnvelope("auth_refresh_error", err.Error()), nil
		}
		return okEnvelope(json.RawMessage(raw)), nil

	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerKey}), nil

	case pluginabi.MethodModelForAuth:
		raw, err := handleModelForAuth(request)
		if err != nil {
			return errorEnvelope("model_for_auth_error", err.Error()), nil
		}
		return okEnvelope(json.RawMessage(raw)), nil

	case pluginabi.MethodModelStatic:
		// The list comes from upstream (cached for modelCatalogueTTL); a config
		// `models:` value pins it. See models.go for the fallback order.
		ids := currentCatalogue().IDs
		models := make([]modelInfo, 0, len(ids))
		for _, id := range ids {
			models = append(models, modelInfo{
				ID:          id,
				Object:      "model",
				OwnedBy:     providerKey,
				Type:        "chat",
				DisplayName: displayName(id),
				Name:        id,
			})
		}
		return okEnvelope(modelResponse{Provider: providerKey, Models: models}), nil

	case pluginabi.MethodExecutorExecute:
		if len(request) > envelopeLimit {
			return errorResult(requestError(http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request envelope exceeds the %d byte cap", envelopeLimit))), nil
		}
		raw, err := handleExecute(request)
		if err != nil {
			return errorResult(err), nil
		}
		return okEnvelope(json.RawMessage(raw)), nil

	case pluginabi.MethodExecutorExecuteStream:
		if len(request) > envelopeLimit {
			return errorResult(requestError(http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request envelope exceeds the %d byte cap", envelopeLimit))), nil
		}
		raw, err := handleExecuteStream(request)
		if err != nil {
			return errorResult(err), nil
		}
		return okEnvelope(json.RawMessage(raw)), nil

	default:
		return errorEnvelope("unknown_method", fmt.Sprintf("unknown method: %s", method)), nil
	}
}

// displayName turns "gemini-4-argon" into "Gemini 4 Argon".
//
// #FUNC     Humanise one model id for the wire.
// #EDGE-IN  a multi-byte first rune ("é-model", "日本-model") → decoded as a rune
// #NOT      byte slicing here produced invalid UTF-8 in the display name, which
//
//	the Claude-format model list serves to clients as display_name
func displayName(id string) string {
	parts := strings.Split(id, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		first, size := utf8.DecodeRuneInString(p)
		if first == utf8.RuneError && size <= 1 {
			continue
		}
		parts[i] = strings.ToUpper(string(first)) + p[size:]
	}
	return strings.Join(parts, " ")
}

// pluginRegistration advertises the two capabilities this build implements.
func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Go Adapter",
			Version:          pluginVersion,
			Author:           "MISTYEYE2088",
			GitHubRepository: "https://github.com/MISTYEYE2088/opencode-go-reverse",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "base-url", Type: pluginapi.ConfigFieldTypeString, Default: defaultBaseURL, Description: "Inference base URL; /zen/go/v1 is the legacy alias."},
				{Name: "user-agent", Type: pluginapi.ConfigFieldTypeString, Default: defaultUserAgent, Description: "User-Agent sent upstream."},
				{Name: "models", Type: pluginapi.ConfigFieldTypeString, Description: "Optional comma-separated model list; empty fetches the catalogue from the gateway."},
			},
		},
		Capabilities: capabilities{
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeBoth),
			ExecutorInputFormats:  []string{"openai"},
			ExecutorOutputFormats: []string{"openai"},
			ModelProvider:         true,
			Scheduler:             true,
			QuotaProvider:         true,
			ManagementAPI:         true,
		},
	}
}
