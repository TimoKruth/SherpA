package collector

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type HTTPConfig struct {
	TokenDigest [32]byte
	MaxBytes    int64
}

type collectorHandler struct {
	config   HTTPConfig
	ingestor Ingestor
	status   *StatusTracker
	logger   *log.Logger
	admit    chan struct{}
}

type uploadResponse struct {
	ObjectID string       `json:"object_id"`
	Status   ResultStatus `json:"status"`
}

func NewHandler(config HTTPConfig, ingestor Ingestor, status *StatusTracker, now func() time.Time, startupGrace, maxRecoveryAge time.Duration, logger *log.Logger) http.Handler {
	_ = now
	_ = startupGrace
	_ = maxRecoveryAge
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &collectorHandler{
		config: config, ingestor: ingestor, status: status, logger: logger, admit: make(chan struct{}, 1),
	}
}

func (handler *collectorHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/v1/exports":
		handler.upload(response, request)
	case "/healthz":
		handler.health(response, request)
	case "/readyz":
		handler.ready(response, request)
	default:
		http.NotFound(response, request)
	}
}

func (handler *collectorHandler) upload(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		response.Header().Set("Allow", http.MethodPost)
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !handler.authorized(request.Header.Values("Authorization")) {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	if len(request.TransferEncoding) != 0 {
		http.Error(response, "invalid request framing", http.StatusBadRequest)
		return
	}
	if request.ContentLength <= 0 {
		http.Error(response, "content length required", http.StatusLengthRequired)
		return
	}
	if handler.config.MaxBytes <= 0 || request.ContentLength > handler.config.MaxBytes {
		http.Error(response, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	contentTypes := request.Header.Values("Content-Type")
	if len(contentTypes) != 1 || contentTypes[0] != "application/gzip" {
		http.Error(response, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	select {
	case handler.admit <- struct{}{}:
		defer func() { <-handler.admit }()
	default:
		http.Error(response, "upload unavailable", http.StatusServiceUnavailable)
		return
	}

	result, err := handler.ingestor.Ingest(request.Context(), request.Body, request.ContentLength)
	if err != nil {
		status, class := classifyUploadError(err)
		handler.logger.Printf("collector upload failed class=%s", class)
		http.Error(response, "collector upload failed", status)
		return
	}
	if !validUploadResult(result) {
		handler.logger.Print("collector upload failed class=internal")
		http.Error(response, "collector upload failed", http.StatusInternalServerError)
		return
	}
	status := http.StatusCreated
	if result.Status == ResultExisting {
		status = http.StatusOK
	}
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(uploadResponse{ObjectID: result.ObjectID, Status: result.Status})
}

func validUploadResult(result Result) bool {
	if result.Status != ResultStored && result.Status != ResultExisting {
		return false
	}
	const prefix = "sha256:"
	if !strings.HasPrefix(result.ObjectID, prefix) || len(result.ObjectID) != len(prefix)+sha256.Size*2 {
		return false
	}
	digest := strings.TrimPrefix(result.ObjectID, prefix)
	if digest != strings.ToLower(digest) {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func classifyUploadError(err error) (int, string) {
	if err == nil {
		return http.StatusInternalServerError, "internal"
	}
	switch err.Error() {
	case "collector archive invalid",
		"collector upload invalid",
		"collector upload length mismatch",
		"collector upload source not closable",
		"collector upload source close failed":
		return http.StatusUnprocessableEntity, "validation"
	case "collector insufficient storage":
		return http.StatusInsufficientStorage, "storage"
	case "collector backend timeout",
		"collector backend unavailable",
		"collector backend cancelled",
		"collector remote verification failed",
		"collector ingestion canceled",
		"collector upload canceled":
		return http.StatusServiceUnavailable, "backend"
	default:
		return http.StatusInternalServerError, "internal"
	}
}

func (handler *collectorHandler) authorized(values []string) bool {
	if len(values) != 1 {
		return false
	}
	value := values[0]
	if !strings.HasPrefix(value, "Bearer ") {
		return false
	}
	candidate := strings.TrimPrefix(value, "Bearer ")
	if candidate == "" || strings.ContainsAny(candidate, ", \t\r\n") {
		return false
	}
	digest := sha256.Sum256([]byte(candidate))
	return subtle.ConstantTimeCompare(digest[:], handler.config.TokenDigest[:]) == 1
}

func (handler *collectorHandler) health(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(response, "ok\n")
}

func (handler *collectorHandler) ready(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if handler.status == nil || !handler.status.Ready() {
		response.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(response, "not ready\n")
		return
	}
	response.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(response, "ready\n")
}
