package api

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
	"regexp"
	"runtime/debug"
	"time"
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type requestIDContextKey struct{}

func requestMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		id := request.Header.Get("X-Request-ID")
		if !requestIDPattern.MatchString(id) {
			id = randomID()
		}
		request = request.WithContext(
			context.WithValue(request.Context(), requestIDContextKey{}, id),
		)

		recorder := &statusRecorder{ResponseWriter: writer}
		recorder.Header().Set("X-Request-ID", id)
		recorder.Header().Set("X-Content-Type-Options", "nosniff")
		recorder.Header().Set("Cache-Control", "no-store")
		started := time.Now()

		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Error(
					"panic while serving request",
					"request_id", id,
					"method", request.Method,
					"path", request.URL.Path,
					"panic", recovered,
					"stack", string(debug.Stack()),
				)
				if !recorder.wroteHeader {
					writeProblem(
						recorder,
						request,
						http.StatusInternalServerError,
						"INTERNAL_ERROR",
						"internal server error",
					)
				}
			}

			logger.Info(
				"http request",
				"request_id", id,
				"method", request.Method,
				"path", request.URL.Path,
				"status", recorder.statusCode(),
				"bytes", recorder.bytes,
				"duration", time.Since(started),
			)
		}()

		next.ServeHTTP(recorder, request)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	count, err := r.ResponseWriter.Write(data)
	r.bytes += count
	return count, err
}

func (r *statusRecorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeProblem(
	writer http.ResponseWriter,
	request *http.Request,
	status int,
	code, message string,
) {
	writeJSON(writer, status, ErrorResponse{
		Code:      code,
		Error:     message,
		RequestID: requestID(request.Context()),
	})
}

func decodeJSON(
	writer http.ResponseWriter,
	request *http.Request,
	target any,
	allowEmpty bool,
) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, maxJSONBodySize)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		if allowEmpty && errors.Is(err, io.EOF) {
			return true
		}
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeProblem(
				writer,
				request,
				http.StatusRequestEntityTooLarge,
				"JSON_BODY_TOO_LARGE",
				"JSON request exceeds the upload limit",
			)
			return false
		}
		writeProblem(
			writer,
			request,
			http.StatusBadRequest,
			"INVALID_JSON",
			"request body must contain one valid JSON object",
		)
		return false
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeProblem(
			writer,
			request,
			http.StatusBadRequest,
			"INVALID_JSON",
			"request body must contain exactly one JSON object",
		)
		return false
	}

	return true
}

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDContextKey{}).(string)
	return id
}

func randomID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(fmt.Sprintf("generate secure id: %v", err))
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80

	var encoded [36]byte
	hex.Encode(encoded[0:8], value[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], value[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], value[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], value[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], value[10:16])

	return string(encoded[:])
}
