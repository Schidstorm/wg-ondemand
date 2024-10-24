package aws

import (
	"strings"

	"github.com/aws/smithy-go/logging"
	"github.com/charmbracelet/log"
)

type AwsLogger struct {
	baseLogger *log.Logger
}

func NewAwsLogger(baseLogger *log.Logger) *AwsLogger {
	return &AwsLogger{baseLogger: baseLogger.WithPrefix("AWS SDK")}
}

func (l AwsLogger) Logf(classification logging.Classification, format string, v ...interface{}) {
	var logLevel log.Level
	switch classification {
	case logging.Warn:
		logLevel = log.WarnLevel
	case logging.Debug:
		logLevel = log.DebugLevel
	default:
		logLevel = log.InfoLevel
	}

	if len(v) == 0 {
		l.baseLogger.Logf(logLevel, format)
		return
	}

	v = l.redactAuthorization(v...)
	l.baseLogger.Logf(logLevel, format, v...)
}

func (l AwsLogger) redactAuthorization(v ...any) []any {
	result := make([]any, 0, len(v))
	for _, i := range v {
		if s, ok := i.(string); ok {
			lines := strings.Split(s, "\n")
			for i, line := range lines {
				if strings.Contains(line, "Authorization") {
					lines[i] = "Authorization: [REDACTED]"
				}
			}
			result = append(result, strings.Join(lines, "\n"))
		} else {
			result = append(result, i)
		}
	}

	return result
}
