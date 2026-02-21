package logger

import (
	"log/slog"
	"os"
	"time"

	"github.com/lmittmann/tint"
)

var Log *slog.Logger

func Init(debug bool) {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}

	Log = slog.New(tint.NewHandler(os.Stdout, &tint.Options{
		Level:      level,
		TimeFormat: time.RFC3339,
	}))
	slog.SetDefault(Log)
}

func Debug(msg string, args ...any) {
	Log.Debug(msg, args...)
}

func Info(msg string, args ...any) {
	Log.Info(msg, args...)
}

func Warn(msg string, args ...any) {
	Log.Warn(msg, args...)
}

func Error(msg string, args ...any) {
	Log.Error(msg, args...)
}
