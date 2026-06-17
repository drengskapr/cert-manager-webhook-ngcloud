package main

import (
	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/component-base/logs/json"
)

// jsonRFC3339Format is the name of the custom log format registered by this
// package. It behaves like component-base's built-in "json" format but encodes
// timestamps as RFC3339Nano strings instead of epoch milliseconds.
const jsonRFC3339Format = "json-rfc3339"

// rfc3339JSONFactory creates JSON loggers whose "ts" field is an RFC3339Nano
// string (e.g. "2026-06-17T12:46:13.398936Z"). It mirrors the default,
// non-split-stream behavior of component-base's json.Factory, overriding only
// the time encoder; component-base's json format hardcodes epoch-millis and
// exposes no option to change it.
type rfc3339JSONFactory struct{}

func (rfc3339JSONFactory) Create(c logsapi.LoggingConfiguration, o logsapi.LoggingOptions) (logr.Logger, logsapi.RuntimeControl) {
	encoderConfig := &zapcore.EncoderConfig{
		MessageKey:     "msg",
		CallerKey:      "caller",
		NameKey:        "logger",
		TimeKey:        "ts",
		EncodeTime:     zapcore.RFC3339NanoTimeEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}
	// Write info and error messages to a single stream, matching the json
	// factory's default (non-split) behavior.
	stream := zapcore.Lock(json.AddNopSync(o.ErrorStream))
	return json.NewJSONLogger(c.Verbosity, stream, nil, encoderConfig)
}

// registerJSONRFC3339Format registers the json-rfc3339 log format with
// component-base. It must be called before the logging flags are parsed: the
// format registry is frozen when the framework builds its flag set. It reuses
// the LoggingBetaOptions feature gate that already gates the built-in json
// format, so no additional gate needs to be enabled.
func registerJSONRFC3339Format() error {
	return logsapi.RegisterLogFormat(jsonRFC3339Format, rfc3339JSONFactory{}, logsapi.LoggingBetaOptions)
}
