// Copyright 2017 Pilosa Corp.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package http

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"expvar"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	// Imported for its side-effect of registering pprof endpoints with the server.
	_ "net/http/pprof"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/pilosa/pilosa"
	"github.com/pilosa/pilosa/internal"

	"github.com/pkg/errors"
)

// Handler represents an HTTP handler.
type Handler struct {
	Handler http.Handler

	Logger pilosa.Logger

	// Keeps the query argument validators for each handler
	validators map[string]*queryValidationSpec

	API *pilosa.API

	AllowedOrigins []string

	ln net.Listener

	server *http.Server
}

// externalPrefixFlag denotes endpoints that are intended to be exposed to clients.
// This is used for stats tagging.
var externalPrefixFlag = map[string]bool{
	"schema":  true,
	"query":   true,
	"import":  true,
	"export":  true,
	"index":   true,
	"field":   true,
	"nodes":   true,
	"version": true,
}

type errorResponse struct {
	Error string `json:"error"`
}

// HandlerOption is a functional option type for pilosa.Handler
type HandlerOption func(s *Handler) error

func OptHandlerAllowedOrigins(origins []string) HandlerOption {
	return func(h *Handler) error {
		h.Handler = handlers.CORS(
			handlers.AllowedOrigins(origins),
			handlers.AllowedHeaders([]string{"Content-Type"}),
		)(h.Handler)
		return nil
	}
}

func OptHandlerAPI(api *pilosa.API) HandlerOption {
	return func(h *Handler) error {
		h.API = api
		return nil
	}
}

func OptHandlerLogger(logger pilosa.Logger) HandlerOption {
	return func(h *Handler) error {
		h.Logger = logger
		return nil
	}
}

func OptHandlerListener(ln net.Listener) HandlerOption {
	return func(h *Handler) error {
		h.ln = ln
		return nil
	}
}

// NewHandler returns a new instance of Handler with a default logger.
func NewHandler(opts ...HandlerOption) (*Handler, error) {
	handler := &Handler{
		Logger: pilosa.NopLogger,
	}
	handler.Handler = NewRouter(handler)
	handler.populateValidators()

	for _, opt := range opts {
		err := opt(handler)
		if err != nil {
			return nil, errors.Wrap(err, "applying option")
		}
	}

	if handler.API == nil {
		return nil, errors.New("must pass OptHandlerAPI")
	}

	if handler.ln == nil {
		return nil, errors.New("must pass OptHandlerListener")
	}

	handler.server = &http.Server{Handler: handler}

	return handler, nil
}

func (h *Handler) Serve() error {
	err := h.server.Serve(h.ln)
	if err != nil && err.Error() != "http: Server closed" {
		h.Logger.Printf("HTTP handler terminated with error: %s\n", err)
		return errors.Wrap(err, "serve http")
	}
	return nil
}

func (h *Handler) Close() error {
	// TODO: timeout?
	err := h.server.Shutdown(context.Background())
	return errors.Wrap(err, "shutdown http server")
}

func (h *Handler) populateValidators() {
	h.validators = map[string]*queryValidationSpec{}
	h.validators["GetFragmentNodes"] = queryValidationSpecRequired("shard", "index")
	h.validators["GetShardMax"] = queryValidationSpecRequired()
	h.validators["PostQuery"] = queryValidationSpecRequired().Optional("shards", "columnAttrs", "excludeRowAttrs", "excludeColumns")
	h.validators["GetExport"] = queryValidationSpecRequired("index", "field", "shard")
	h.validators["GetFragmentData"] = queryValidationSpecRequired("index", "field", "shard")
	h.validators["PostFragmentData"] = queryValidationSpecRequired("index", "field", "shard")
	h.validators["GetFragmentBlocks"] = queryValidationSpecRequired("index", "field", "shard")
}

func (h *Handler) queryArgValidator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := mux.CurrentRoute(r).GetName()
		if validator, ok := h.validators[key]; ok {
			if err := validator.validate(r.URL.Query()); err != nil {
				// TODO: Return the response depending on the Accept header
				response := errorResponse{Error: err.Error()}
				body, err := json.Marshal(response)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				http.Error(w, string(body), http.StatusBadRequest)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// NewRouter creates a new mux http router.
func NewRouter(handler *Handler) *mux.Router {
	router := mux.NewRouter()
	router.HandleFunc("/", handler.handleHome).Methods("GET")
	router.HandleFunc("/cluster/resize/abort", handler.handlePostClusterResizeAbort).Methods("POST")
	router.HandleFunc("/cluster/resize/remove-node", handler.handlePostClusterResizeRemoveNode).Methods("POST")
	router.HandleFunc("/cluster/resize/set-coordinator", handler.handlePostClusterResizeSetCoordinator).Methods("POST")
	router.PathPrefix("/debug/pprof/").Handler(http.DefaultServeMux).Methods("GET")
	router.Handle("/debug/vars", expvar.Handler()).Methods("GET")
	router.HandleFunc("/export", handler.handleGetExport).Methods("GET").Name("GetExport")
	router.HandleFunc("/index", handler.handleGetIndexes).Methods("GET")
	router.HandleFunc("/index/{index}", handler.handleGetIndex).Methods("GET")
	router.HandleFunc("/index/{index}", handler.handlePostIndex).Methods("POST")
	router.HandleFunc("/index/{index}", handler.handleDeleteIndex).Methods("DELETE")
	//router.HandleFunc("/index/{index}/field", handler.handleGetFields).Methods("GET") // Not implemented.
	router.HandleFunc("/index/{index}/field/{field}", handler.handlePostField).Methods("POST")
	router.HandleFunc("/index/{index}/field/{field}", handler.handleDeleteField).Methods("DELETE")
	router.HandleFunc("/index/{index}/field/{field}/import", handler.handlePostImport).Methods("POST")
	router.HandleFunc("/index/{index}/query", handler.handlePostQuery).Methods("POST").Name("PostQuery")
	router.HandleFunc("/info", handler.handleGetInfo).Methods("GET")
	router.HandleFunc("/recalculate-caches", handler.handleRecalculateCaches).Methods("POST")
	router.HandleFunc("/schema", handler.handleGetSchema).Methods("GET")
	router.HandleFunc("/status", handler.handleGetStatus).Methods("GET")
	router.HandleFunc("/version", handler.handleGetVersion).Methods("GET")

	// /internal endpoints are for internal use only; they may change at any time.
	// DO NOT rely on these for external applications!
	router.HandleFunc("/internal/cluster/message", handler.handlePostClusterMessage).Methods("POST")
	router.HandleFunc("/internal/fragment/block/data", handler.handleGetFragmentBlockData).Methods("GET")
	router.HandleFunc("/internal/fragment/blocks", handler.handleGetFragmentBlocks).Methods("GET").Name("GetFragmentBlocks")
	router.HandleFunc("/internal/fragment/nodes", handler.handleGetFragmentNodes).Methods("GET").Name("GetFragmentNodes")
	router.HandleFunc("/internal/index/{index}/attr/diff", handler.handlePostIndexAttrDiff).Methods("POST")
	router.HandleFunc("/internal/index/{index}/field/{field}/attr/diff", handler.handlePostFieldAttrDiff).Methods("POST")
	router.HandleFunc("/internal/shards/max", handler.handleGetShardsMax).Methods("GET") // TODO: deprecate, but it's being used by the client
	router.HandleFunc("/internal/translate/data", handler.handleGetTranslateData).Methods("GET")

	// TODO: Apply MethodNotAllowed statuses to all endpoints.
	// Ideally this would be automatic, as described in this (wontfix) ticket:
	// https://github.com/gorilla/mux/issues/6
	// For now we just do it for the most commonly used handler, /query
	router.HandleFunc("/index/{index}/query", handler.methodNotAllowedHandler).Methods("GET")

	router.Use(handler.queryArgValidator)
	return router
}

func (h *Handler) methodNotAllowedHandler(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

// ServeHTTP handles an HTTP request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if err := recover(); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			stack := debug.Stack()
			msg := "PANIC: %s\n%s"
			h.Logger.Printf(msg, err, stack)
			fmt.Fprintf(w, msg, err, stack)
		}
	}()

	t := time.Now()
	h.Handler.ServeHTTP(w, r)
	dif := time.Since(t)

	// Calculate per request StatsD metrics when the handler is fully configured.
	statsTags := make([]string, 0, 3)

	longQueryTime := h.API.LongQueryTime()
	if longQueryTime > 0 && dif > longQueryTime {
		h.Logger.Printf("%s %s %v", r.Method, r.URL.String(), dif)
		statsTags = append(statsTags, "slow_query")
	}

	pathParts := strings.Split(r.URL.Path, "/")
	endpointName := strings.Join(pathParts, "_")

	if externalPrefixFlag[pathParts[1]] {
		statsTags = append(statsTags, "external")
	}

	// useragent tag identifies internal/external endpoints
	statsTags = append(statsTags, "useragent:"+r.UserAgent())
	stats := h.API.StatsWithTags(statsTags)
	if stats != nil {
		stats.Histogram("http."+endpointName, float64(dif), 0.1)
	}
}

// successResponse is a general success/error struct for http responses.
type successResponse struct {
	Success bool   `json:"success"`
	Error   *Error `json:"error,omitempty"`
}

// check determines success or failure based on the error.
// It also returns the corresponding http status code.
func (r *successResponse) check(err error) (statusCode int) {
	if err == nil {
		r.Success = true
		return
	}

	cause := errors.Cause(err)

	// Determine HTTP status code based on the error type.
	switch cause.(type) {
	case pilosa.BadRequestError:
		statusCode = http.StatusBadRequest
	case pilosa.ConflictError:
		statusCode = http.StatusConflict
	case pilosa.NotFoundError:
		statusCode = http.StatusNotFound
	default:
		statusCode = http.StatusInternalServerError
	}

	r.Success = false
	r.Error = &Error{Message: cause.Error()}

	return
}

// write sends a response to the http.ResponseWriter based on the success
// status and the error.
func (r *successResponse) write(w http.ResponseWriter, err error) {
	// Apply the error and get the status code.
	statusCode := r.check(err)

	// Marshal the json response.
	msg, err := json.Marshal(r)
	if err != nil {
		http.Error(w, string(msg), http.StatusInternalServerError)
		return
	}

	// Write the response.
	if statusCode == 0 {
		w.Write(msg)
		w.Write([]byte("\n"))
	} else {
		http.Error(w, string(msg), statusCode)
	}
}

func (h *Handler) handleHome(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "Welcome. Pilosa is running. Visit https://www.pilosa.com/docs/ for more information.", http.StatusNotFound)
}

// validHeaderAcceptJSON returns false if one or more Accept
// headers are present, but none of them are "application/json"
// (or any matching wildcard). Otherwise returns true.
func validHeaderAcceptJSON(header http.Header) bool {
	if v, found := header["Accept"]; found {
		for _, v := range v {
			if v == "application/json" || v == "*/*" || v == "*/json" || v == "application/*" {
				return true
			}
		}
		return false
	}
	return true
}

// handleGetSchema handles GET /schema requests.
func (h *Handler) handleGetSchema(w http.ResponseWriter, r *http.Request) {
	if !validHeaderAcceptJSON(r.Header) {
		http.Error(w, "JSON only acceptable response", http.StatusNotAcceptable)
		return
	}

	schema := h.API.Schema(r.Context())
	if err := json.NewEncoder(w).Encode(map[string]interface{}{"indexes": schema}); err != nil {
		h.Logger.Printf("write schema response error: %s", err)
	}
}

// handleGetStatus handles GET /status requests.
func (h *Handler) handleGetStatus(w http.ResponseWriter, r *http.Request) {
	if !validHeaderAcceptJSON(r.Header) {
		http.Error(w, "JSON only acceptable response", http.StatusNotAcceptable)
		return
	}
	status := getStatusResponse{
		State:   h.API.State(),
		Nodes:   h.API.Hosts(r.Context()),
		LocalID: h.API.Node().ID,
	}
	if err := json.NewEncoder(w).Encode(status); err != nil {
		h.Logger.Printf("write status response error: %s", err)
	}
}

func (h *Handler) handleGetInfo(w http.ResponseWriter, r *http.Request) {
	if !validHeaderAcceptJSON(r.Header) {
		http.Error(w, "JSON only acceptable response", http.StatusNotAcceptable)
		return
	}
	info := h.API.Info()
	if err := json.NewEncoder(w).Encode(info); err != nil {
		h.Logger.Printf("write info response error: %s", err)
	}
}

type getSchemaResponse struct {
	Indexes []*pilosa.IndexInfo `json:"indexes"`
}

type getStatusResponse struct {
	State   string         `json:"state"`
	Nodes   []*pilosa.Node `json:"nodes"`
	LocalID string         `json:"localID"`
}

// handlePostQuery handles /query requests.
func (h *Handler) handlePostQuery(w http.ResponseWriter, r *http.Request) {
	// Parse incoming request.
	req, err := h.readQueryRequest(r)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		h.writeQueryResponse(w, r, &pilosa.QueryResponse{Err: err})
		return
	}
	// TODO: Remove
	req.Index = mux.Vars(r)["index"]

	resp, err := h.API.Query(r.Context(), req)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		h.writeQueryResponse(w, r, &pilosa.QueryResponse{Err: err})
		return
	}

	// Set appropriate status code, if there is an error.
	if resp.Err != nil {
		switch resp.Err {
		case pilosa.ErrTooManyWrites:
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}

	// Write response back to client.
	if err := h.writeQueryResponse(w, r, &resp); err != nil {
		h.Logger.Printf("write query response error: %s", err)
	}
}

// handleGetShardsMax handles GET /internal/shards/max requests.
func (h *Handler) handleGetShardsMax(w http.ResponseWriter, r *http.Request) {
	if !validHeaderAcceptJSON(r.Header) {
		http.Error(w, "JSON only acceptable response", http.StatusNotAcceptable)
		return
	}
	if err := json.NewEncoder(w).Encode(getShardsMaxResponse{
		Standard: h.API.MaxShards(r.Context()),
	}); err != nil {
		h.Logger.Printf("write shards-max response error: %s", err)
	}
}

type getShardsMaxResponse struct {
	Standard map[string]uint64 `json:"standard"`
}

// handleGetIndexes handles GET /index request.
func (h *Handler) handleGetIndexes(w http.ResponseWriter, r *http.Request) {
	h.handleGetSchema(w, r)
}

// handleGetIndex handles GET /index/<indexname> requests.
func (h *Handler) handleGetIndex(w http.ResponseWriter, r *http.Request) {
	if !validHeaderAcceptJSON(r.Header) {
		http.Error(w, "JSON only acceptable response", http.StatusNotAcceptable)
		return
	}
	indexName := mux.Vars(r)["index"]
	for _, idx := range h.API.Schema(r.Context()) {
		if idx.Name() == indexName {
			if err := json.NewEncoder(w).Encode(idx); err != nil {
				h.Logger.Printf("write response error: %s", err)
			}
			return
		}
	}
	http.Error(w, fmt.Sprintf("Index %s Not Found", indexName), http.StatusNotFound)
}

type postIndexRequest struct {
	Options pilosa.IndexOptions `json:"options"`
}

//_postIndexRequest is necessary to avoid recursion while decoding.
type _postIndexRequest postIndexRequest

// Custom Unmarshal JSON to validate request body when creating a new index.
func (p *postIndexRequest) UnmarshalJSON(b []byte) error {

	// m is an overflow map used to capture additional, unexpected keys.
	m := make(map[string]interface{})
	if err := json.Unmarshal(b, &m); err != nil {
		return errors.Wrap(err, "unmarshalling unexpected values")
	}

	validIndexOptions := getValidOptions(pilosa.IndexOptions{})
	err := validateOptions(m, validIndexOptions)
	if err != nil {
		return err
	}
	// Unmarshal expected values.
	var _p _postIndexRequest
	if err := json.Unmarshal(b, &_p); err != nil {
		return errors.Wrap(err, "unmarshalling expected values")
	}

	p.Options = _p.Options

	return nil
}

func getValidOptions(option interface{}) []string {
	validOptions := []string{}
	val := reflect.ValueOf(option)
	for i := 0; i < val.Type().NumField(); i++ {
		jsonTag := val.Type().Field(i).Tag.Get("json")
		s := strings.Split(jsonTag, ",")
		validOptions = append(validOptions, s[0])
	}
	return validOptions
}

// Raise errors for any unknown key
func validateOptions(data map[string]interface{}, validIndexOptions []string) error {
	for k, v := range data {
		switch k {
		case "options":
			options, ok := v.(map[string]interface{})
			if !ok {
				return errors.New("options is not map[string]interface{}")
			}
			for kk, vv := range options {
				if !foundItem(validIndexOptions, kk) {
					return fmt.Errorf("Unknown key: %v:%v", kk, vv)
				}
			}
		default:
			return fmt.Errorf("Unknown key: %v:%v", k, v)
		}
	}
	return nil
}

func foundItem(items []string, item string) bool {
	for _, i := range items {
		if item == i {
			return true
		}
	}
	return false
}

// handleDeleteIndex handles DELETE /index request.
func (h *Handler) handleDeleteIndex(w http.ResponseWriter, r *http.Request) {
	if !validHeaderAcceptJSON(r.Header) {
		http.Error(w, "JSON only acceptable response", http.StatusNotAcceptable)
		return
	}

	indexName := mux.Vars(r)["index"]

	resp := successResponse{}
	err := h.API.DeleteIndex(r.Context(), indexName)
	resp.write(w, err)
}

// handlePostIndex handles POST /index request.
func (h *Handler) handlePostIndex(w http.ResponseWriter, r *http.Request) {
	if !validHeaderAcceptJSON(r.Header) {
		http.Error(w, "JSON only acceptable response", http.StatusNotAcceptable)
		return
	}
	indexName := mux.Vars(r)["index"]

	resp := successResponse{}

	// Decode request.
	var req postIndexRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil && err != io.EOF {
		resp.write(w, err)
		return
	}
	_, err = h.API.CreateIndex(r.Context(), indexName, req.Options)

	resp.write(w, err)
}

// handlePostIndexAttrDiff handles POST /internal/index/attr/diff requests.
func (h *Handler) handlePostIndexAttrDiff(w http.ResponseWriter, r *http.Request) {
	if !validHeaderAcceptJSON(r.Header) {
		http.Error(w, "JSON only acceptable response", http.StatusNotAcceptable)
		return
	}
	indexName := mux.Vars(r)["index"]

	// Decode request.
	var req postIndexAttrDiffRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	attrs, err := h.API.IndexAttrDiff(r.Context(), indexName, req.Blocks)
	if err != nil {
		if errors.Cause(err) == pilosa.ErrIndexNotFound {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// Encode response.
	if err := json.NewEncoder(w).Encode(postIndexAttrDiffResponse{
		Attrs: attrs,
	}); err != nil {
		h.Logger.Printf("response encoding error: %s", err)
	}
}

type postIndexAttrDiffRequest struct {
	Blocks []pilosa.AttrBlock `json:"blocks"`
}

type postIndexAttrDiffResponse struct {
	Attrs map[uint64]map[string]interface{} `json:"attrs"`
}

// handlePostField handles POST /field request.
func (h *Handler) handlePostField(w http.ResponseWriter, r *http.Request) {
	if !validHeaderAcceptJSON(r.Header) {
		http.Error(w, "JSON only acceptable response", http.StatusNotAcceptable)
		return
	}
	indexName := mux.Vars(r)["index"]
	fieldName := mux.Vars(r)["field"]

	resp := successResponse{}

	// Decode request.
	var req postFieldRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(&req)
	if err != nil && err != io.EOF {
		resp.write(w, err)
		return
	}

	// Validate field options.
	if err := req.Options.validate(); err != nil {
		resp.write(w, err)
		return
	}

	// Convert json options into functional options.
	var fos pilosa.FieldOption
	switch req.Options.Type {
	case pilosa.FieldTypeSet:
		fos = pilosa.OptFieldTypeSet(*req.Options.CacheType, *req.Options.CacheSize)
	case pilosa.FieldTypeInt:
		fos = pilosa.OptFieldTypeInt(*req.Options.Min, *req.Options.Max)
	case pilosa.FieldTypeTime:
		fos = pilosa.OptFieldTypeTime(*req.Options.TimeQuantum)
	}

	_, err = h.API.CreateField(r.Context(), indexName, fieldName, fos)
	resp.write(w, err)
}

type postFieldRequest struct {
	Options fieldOptions `json:"options"`
}

// fieldOptions tracks pilosa.FieldOptions. It is made up of pointers to values,
// and used for input validation.
type fieldOptions struct {
	Type        string              `json:"type,omitempty"`
	CacheType   *string             `json:"cacheType,omitempty"`
	CacheSize   *uint32             `json:"cacheSize,omitempty"`
	Min         *int64              `json:"min,omitempty"`
	Max         *int64              `json:"max,omitempty"`
	TimeQuantum *pilosa.TimeQuantum `json:"timeQuantum,omitempty"`
	Keys        *bool               `json:"keys,omitempty"`
}

func (o *fieldOptions) validate() error {
	// Pointers to default values.
	defaultCacheType := pilosa.DefaultCacheType
	defaultCacheSize := uint32(pilosa.DefaultCacheSize)

	switch o.Type {
	case pilosa.FieldTypeSet, "":
		// Because FieldTypeSet is the default, its arguments are
		// not required. Instead, the defaults are applied whenever
		// a value does not exist.
		if o.Type == "" {
			o.Type = pilosa.FieldTypeSet
		}
		if o.CacheType == nil {
			o.CacheType = &defaultCacheType
		}
		if o.CacheSize == nil {
			o.CacheSize = &defaultCacheSize
		}
		if o.Min != nil {
			return pilosa.NewBadRequestError(errors.New("min does not apply to field type set"))
		} else if o.Max != nil {
			return pilosa.NewBadRequestError(errors.New("max does not apply to field type set"))
		} else if o.TimeQuantum != nil {
			return pilosa.NewBadRequestError(errors.New("timeQuantum does not apply to field type set"))
		}
	case pilosa.FieldTypeInt:
		if o.CacheType != nil {
			return pilosa.NewBadRequestError(errors.New("cacheType does not apply to field type int"))
		} else if o.CacheSize != nil {
			return pilosa.NewBadRequestError(errors.New("cacheSize does not apply to field type int"))
		} else if o.Min == nil {
			return pilosa.NewBadRequestError(errors.New("min is required for field type int"))
		} else if o.Max == nil {
			return pilosa.NewBadRequestError(errors.New("max is required for field type int"))
		} else if o.TimeQuantum != nil {
			return pilosa.NewBadRequestError(errors.New("timeQuantum does not apply to field type int"))
		}
	case pilosa.FieldTypeTime:
		if o.CacheType != nil {
			return pilosa.NewBadRequestError(errors.New("cacheType does not apply to field type time"))
		} else if o.CacheSize != nil {
			return pilosa.NewBadRequestError(errors.New("cacheSize does not apply to field type time"))
		} else if o.Min != nil {
			return pilosa.NewBadRequestError(errors.New("min does not apply to field type time"))
		} else if o.Max != nil {
			return pilosa.NewBadRequestError(errors.New("max does not apply to field type time"))
		} else if o.TimeQuantum == nil {
			return pilosa.NewBadRequestError(errors.New("timeQuantum is required for field type time"))
		}
	default:
		return errors.Errorf("invalid field type: %s", o.Type)
	}
	return nil
}

// handleDeleteField handles DELETE /field request.
func (h *Handler) handleDeleteField(w http.ResponseWriter, r *http.Request) {
	if !validHeaderAcceptJSON(r.Header) {
		http.Error(w, "JSON only acceptable response", http.StatusNotAcceptable)
		return
	}

	indexName := mux.Vars(r)["index"]
	fieldName := mux.Vars(r)["field"]

	resp := successResponse{}
	err := h.API.DeleteField(r.Context(), indexName, fieldName)
	resp.write(w, err)
}

// handlePostFieldAttrDiff handles POST /internal/field/attr/diff requests.
func (h *Handler) handlePostFieldAttrDiff(w http.ResponseWriter, r *http.Request) {
	if !validHeaderAcceptJSON(r.Header) {
		http.Error(w, "JSON only acceptable response", http.StatusNotAcceptable)
		return
	}
	indexName := mux.Vars(r)["index"]
	fieldName := mux.Vars(r)["field"]

	// Decode request.
	var req postFieldAttrDiffRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	attrs, err := h.API.FieldAttrDiff(r.Context(), indexName, fieldName, req.Blocks)
	if err != nil {
		switch errors.Cause(err) {
		case pilosa.ErrFragmentNotFound:
			http.Error(w, err.Error(), http.StatusNotFound)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// Encode response.
	if err := json.NewEncoder(w).Encode(postFieldAttrDiffResponse{
		Attrs: attrs,
	}); err != nil {
		h.Logger.Printf("response encoding error: %s", err)
	}
}

type postFieldAttrDiffRequest struct {
	Blocks []pilosa.AttrBlock `json:"blocks"`
}

type postFieldAttrDiffResponse struct {
	Attrs map[uint64]map[string]interface{} `json:"attrs"`
}

// readQueryRequest parses an query parameters from r.
func (h *Handler) readQueryRequest(r *http.Request) (*pilosa.QueryRequest, error) {
	switch r.Header.Get("Content-Type") {
	case "application/x-protobuf":
		return h.readProtobufQueryRequest(r)
	default:
		return h.readURLQueryRequest(r)
	}
}

// readProtobufQueryRequest parses query parameters in protobuf from r.
func (h *Handler) readProtobufQueryRequest(r *http.Request) (*pilosa.QueryRequest, error) {
	// Slurp the body.
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, errors.Wrap(err, "reading")
	}

	// Unmarshal into object.
	var req internal.QueryRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		return nil, errors.Wrap(err, "unmarshalling")
	}

	return decodeQueryRequest(&req), nil
}

// readURLQueryRequest parses query parameters from URL parameters from r.
func (h *Handler) readURLQueryRequest(r *http.Request) (*pilosa.QueryRequest, error) {
	q := r.URL.Query()

	// Parse query string.
	buf, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, errors.Wrap(err, "reading")
	}
	query := string(buf)

	// Parse list of shards.
	shards, err := parseUint64Slice(q.Get("shards"))
	if err != nil {
		return nil, errors.New("invalid shard argument")
	}

	return &pilosa.QueryRequest{
		Query:           query,
		Shards:          shards,
		ColumnAttrs:     q.Get("columnAttrs") == "true",
		ExcludeRowAttrs: q.Get("excludeRowAttrs") == "true",
		ExcludeColumns:  q.Get("excludeColumns") == "true",
	}, nil
}

// writeQueryResponse writes the response from the executor to w.
func (h *Handler) writeQueryResponse(w http.ResponseWriter, r *http.Request, resp *pilosa.QueryResponse) error {
	if !validHeaderAcceptJSON(r.Header) {
		return h.writeProtobufQueryResponse(w, resp)
	}
	return h.writeJSONQueryResponse(w, resp)
}

// writeProtobufQueryResponse writes the response from the executor to w as protobuf.
func (h *Handler) writeProtobufQueryResponse(w http.ResponseWriter, resp *pilosa.QueryResponse) error {
	if buf, err := proto.Marshal(encodeQueryResponse(resp)); err != nil {
		return errors.Wrap(err, "marshalling")
	} else if _, err := w.Write(buf); err != nil {
		return errors.Wrap(err, "writing")
	}
	return nil
}

// writeJSONQueryResponse writes the response from the executor to w as JSON.
func (h *Handler) writeJSONQueryResponse(w http.ResponseWriter, resp *pilosa.QueryResponse) error {
	return json.NewEncoder(w).Encode(resp)
}

// handlePostImport handles /import requests.
func (h *Handler) handlePostImport(w http.ResponseWriter, r *http.Request) {
	// Verify that request is only communicating over protobufs.
	if r.Header.Get("Content-Type") != "application/x-protobuf" {
		http.Error(w, "Unsupported media type", http.StatusUnsupportedMediaType)
		return
	} else if r.Header.Get("Accept") != "application/x-protobuf" {
		http.Error(w, "Not acceptable", http.StatusNotAcceptable)
		return
	}
	indexName := mux.Vars(r)["index"]
	fieldName := mux.Vars(r)["field"]

	// Get index and field type to determine how to handle the
	// import data.
	field, err := h.API.Field(r.Context(), indexName, fieldName)
	if err != nil {
		switch errors.Cause(err) {
		case pilosa.ErrIndexNotFound:
			fallthrough
		case pilosa.ErrFieldNotFound:
			http.Error(w, err.Error(), http.StatusNotFound)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// Read entire body.
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Unmarshal request based on field type.
	if field.Type() == pilosa.FieldTypeInt {
		// Field type: Int
		// Marshal into request object.
		var req internal.ImportValueRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := h.API.ImportValue(r.Context(), req); err != nil {
			switch errors.Cause(err) {
			case pilosa.ErrClusterDoesNotOwnShard:
				http.Error(w, err.Error(), http.StatusPreconditionFailed)
			default:
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
	} else {
		// Field type: Set, Time
		// Marshal into request object.
		var req internal.ImportRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := h.API.Import(r.Context(), req); err != nil {
			switch errors.Cause(err) {
			case pilosa.ErrClusterDoesNotOwnShard:
				http.Error(w, err.Error(), http.StatusPreconditionFailed)
			default:
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
	}

	// Marshal response object.
	buf, e := proto.Marshal(&internal.ImportResponse{Err: ""})
	if e != nil {
		http.Error(w, fmt.Sprintf("marshal import response"), http.StatusInternalServerError)
		return
	}

	// Write response.
	w.Write(buf)
}

// handleGetExport handles /export requests.
func (h *Handler) handleGetExport(w http.ResponseWriter, r *http.Request) {
	switch r.Header.Get("Accept") {
	case "text/csv":
		h.handleGetExportCSV(w, r)
	default:
		http.Error(w, "Not acceptable", http.StatusNotAcceptable)
	}
}

func (h *Handler) handleGetExportCSV(w http.ResponseWriter, r *http.Request) {
	// Parse query parameters.
	q := r.URL.Query()
	index, field := q.Get("index"), q.Get("field")

	shard, err := strconv.ParseUint(q.Get("shard"), 10, 64)
	if err != nil {
		http.Error(w, "invalid shard", http.StatusBadRequest)
		return
	}

	if err = h.API.ExportCSV(r.Context(), index, field, shard, w); err != nil {
		switch errors.Cause(err) {
		case pilosa.ErrFragmentNotFound:
			break
		case pilosa.ErrClusterDoesNotOwnShard:
			http.Error(w, err.Error(), http.StatusPreconditionFailed)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
}

// handleGetFragmentNodes handles /internal/fragment/nodes requests.
func (h *Handler) handleGetFragmentNodes(w http.ResponseWriter, r *http.Request) {
	if !validHeaderAcceptJSON(r.Header) {
		http.Error(w, "JSON only acceptable response", http.StatusNotAcceptable)
		return
	}
	q := r.URL.Query()
	index := q.Get("index")

	// Read shard parameter.
	shard, err := strconv.ParseUint(q.Get("shard"), 10, 64)
	if err != nil {
		http.Error(w, "shard should be an unsigned integer", http.StatusBadRequest)
		return
	}

	// Retrieve fragment owner nodes.
	nodes, err := h.API.ShardNodes(r.Context(), index, shard)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Write to response.
	if err := json.NewEncoder(w).Encode(nodes); err != nil {
		h.Logger.Printf("json write error: %s", err)
	}
}

// handleGetFragmentBlockData handles GET /internal/fragment/block/data requests.
func (h *Handler) handleGetFragmentBlockData(w http.ResponseWriter, r *http.Request) {
	buf, err := h.API.FragmentBlockData(r.Context(), r.Body)
	if err != nil {
		if _, ok := err.(pilosa.BadRequestError); ok {
			http.Error(w, err.Error(), http.StatusBadRequest)
		} else if errors.Cause(err) == pilosa.ErrFragmentNotFound {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// Write response.
	w.Header().Set("Content-Type", "application/protobuf")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	w.Write(buf)
}

// handleGetFragmentBlocks handles GET /internal/fragment/blocks requests.
func (h *Handler) handleGetFragmentBlocks(w http.ResponseWriter, r *http.Request) {
	if !validHeaderAcceptJSON(r.Header) {
		http.Error(w, "JSON only acceptable response", http.StatusNotAcceptable)
		return
	}
	// Read shard parameter.
	q := r.URL.Query()
	shard, err := strconv.ParseUint(q.Get("shard"), 10, 64)
	if err != nil {
		http.Error(w, "shard required", http.StatusBadRequest)
		return
	}

	blocks, err := h.API.FragmentBlocks(r.Context(), q.Get("index"), q.Get("field"), shard)
	if err != nil {
		if errors.Cause(err) == pilosa.ErrFragmentNotFound {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// Encode response.
	if err := json.NewEncoder(w).Encode(getFragmentBlocksResponse{
		Blocks: blocks,
	}); err != nil {
		h.Logger.Printf("block response encoding error: %s", err)
	}
}

type getFragmentBlocksResponse struct {
	Blocks []pilosa.FragmentBlock `json:"blocks"`
}

// handleGetVersion handles /version requests.
func (h *Handler) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	if !validHeaderAcceptJSON(r.Header) {
		http.Error(w, "JSON only acceptable response", http.StatusNotAcceptable)
		return
	}
	err := json.NewEncoder(w).Encode(struct {
		Version string `json:"version"`
	}{
		Version: h.API.Version(),
	})
	if err != nil {
		h.Logger.Printf("write version response error: %s", err)
	}
}

// QueryResult types.
const (
	QueryResultTypeNil uint32 = iota
	QueryResultTypeRow
	QueryResultTypePairs
	QueryResultTypeValCount
	QueryResultTypeUint64
	QueryResultTypeBool
)

func decodeQueryRequest(pb *internal.QueryRequest) *pilosa.QueryRequest {
	req := &pilosa.QueryRequest{
		Query:           pb.Query,
		Shards:          pb.Shards,
		ColumnAttrs:     pb.ColumnAttrs,
		Remote:          pb.Remote,
		ExcludeRowAttrs: pb.ExcludeRowAttrs,
		ExcludeColumns:  pb.ExcludeColumns,
	}

	return req
}

func encodeQueryResponse(resp *pilosa.QueryResponse) *internal.QueryResponse {
	pb := &internal.QueryResponse{
		Results:        make([]*internal.QueryResult, len(resp.Results)),
		ColumnAttrSets: pilosa.EncodeColumnAttrSets(resp.ColumnAttrSets),
	}

	for i := range resp.Results {
		pb.Results[i] = &internal.QueryResult{}

		switch result := resp.Results[i].(type) {
		case *pilosa.Row:
			pb.Results[i].Type = QueryResultTypeRow
			pb.Results[i].Row = pilosa.EncodeRow(result)
		case []pilosa.Pair:
			pb.Results[i].Type = QueryResultTypePairs
			pb.Results[i].Pairs = pilosa.EncodePairs(result)
		case pilosa.ValCount:
			pb.Results[i].Type = QueryResultTypeValCount
			pb.Results[i].ValCount = pilosa.EncodeValCount(result)
		case uint64:
			pb.Results[i].Type = QueryResultTypeUint64
			pb.Results[i].N = result
		case bool:
			pb.Results[i].Type = QueryResultTypeBool
			pb.Results[i].Changed = result
		case nil:
			pb.Results[i].Type = QueryResultTypeNil
		}
	}

	if resp.Err != nil {
		pb.Err = resp.Err.Error()
	}

	return pb
}

// parseUint64Slice returns a slice of uint64s from a comma-delimited string.
func parseUint64Slice(s string) ([]uint64, error) {
	var a []uint64
	for _, str := range strings.Split(s, ",") {
		// Ignore blanks.
		if str == "" {
			continue
		}

		// Parse number.
		num, err := strconv.ParseUint(str, 10, 64)
		if err != nil {
			return nil, errors.Wrap(err, "parsing int")
		}
		a = append(a, num)
	}
	return a, nil
}

// errorString returns the string representation of err.
func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (h *Handler) handlePostClusterResizeSetCoordinator(w http.ResponseWriter, r *http.Request) {
	if !validHeaderAcceptJSON(r.Header) {
		http.Error(w, "JSON only acceptable response", http.StatusNotAcceptable)
		return
	}
	// Decode request.
	var req setCoordinatorRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, "decoding request "+err.Error(), http.StatusBadRequest)
		return
	}

	oldNode, newNode, err := h.API.SetCoordinator(r.Context(), req.ID)
	if err != nil {
		if errors.Cause(err) == pilosa.ErrNodeIDNotExists {
			http.Error(w, "setting new coordinator: "+err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, "setting new coordinator: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}
	// Encode response.
	if err := json.NewEncoder(w).Encode(setCoordinatorResponse{
		Old: oldNode,
		New: newNode,
	}); err != nil {
		h.Logger.Printf("response encoding error: %s", err)
	}
}

type setCoordinatorRequest struct {
	ID string `json:"id"`
}

type setCoordinatorResponse struct {
	Old *pilosa.Node `json:"old"`
	New *pilosa.Node `json:"new"`
}

// handlePostClusterResizeRemoveNode handles POST /cluster/resize/remove-node request.
func (h *Handler) handlePostClusterResizeRemoveNode(w http.ResponseWriter, r *http.Request) {
	if !validHeaderAcceptJSON(r.Header) {
		http.Error(w, "JSON only acceptable response", http.StatusNotAcceptable)
		return
	}
	// Decode request.
	var req removeNodeRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	removeNode, err := h.API.RemoveNode(req.ID)
	if err != nil {
		if errors.Cause(err) == pilosa.ErrNodeIDNotExists {
			http.Error(w, "removing node: "+err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, "removing node: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// Encode response.
	if err := json.NewEncoder(w).Encode(removeNodeResponse{
		Remove: removeNode,
	}); err != nil {
		h.Logger.Printf("response encoding error: %s", err)
	}
}

type removeNodeRequest struct {
	ID string `json:"id"`
}

type removeNodeResponse struct {
	Remove *pilosa.Node `json:"remove"`
}

// handlePostClusterResizeAbort handles POST /cluster/resize/abort request.
func (h *Handler) handlePostClusterResizeAbort(w http.ResponseWriter, r *http.Request) {
	if !validHeaderAcceptJSON(r.Header) {
		http.Error(w, "JSON only acceptable response", http.StatusNotAcceptable)
		return
	}
	err := h.API.ResizeAbort()
	var msg string
	if err != nil {
		switch errors.Cause(err) {
		case pilosa.ErrNodeNotCoordinator:
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		case pilosa.ErrResizeNotRunning:
			msg = err.Error()
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	// Encode response.
	if err := json.NewEncoder(w).Encode(clusterResizeAbortResponse{
		Info: msg,
	}); err != nil {
		h.Logger.Printf("response encoding error: %s", err)
	}
}

type clusterResizeAbortResponse struct {
	Info string `json:"info"`
}

func (h *Handler) handleRecalculateCaches(w http.ResponseWriter, r *http.Request) {
	err := h.API.RecalculateCaches(r.Context())
	if err != nil {
		http.Error(w, "recalculating caches: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handlePostClusterMessage(w http.ResponseWriter, r *http.Request) {
	if !validHeaderAcceptJSON(r.Header) {
		http.Error(w, "JSON only acceptable response", http.StatusNotAcceptable)
		return
	}
	// Verify that request is only communicating over protobufs.
	if r.Header.Get("Content-Type") != "application/x-protobuf" {
		http.Error(w, "Unsupported media type", http.StatusUnsupportedMediaType)
		return
	}

	err := h.API.ClusterMessage(r.Context(), r.Body)
	if err != nil {
		// TODO this was the previous behavior, but perhaps not everything is a bad request
		http.Error(w, err.Error(), http.StatusBadRequest)
	}

	if err := json.NewEncoder(w).Encode(defaultClusterMessageResponse{}); err != nil {
		h.Logger.Printf("response encoding error: %s", err)
	}
}

func (h *Handler) GetAPI() *pilosa.API {
	return h.API
}

type defaultClusterMessageResponse struct{}

func (h *Handler) handleGetTranslateData(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.ParseInt(q.Get("offset"), 10, 64)

	pipeR, pipeW := io.Pipe()

	err := h.API.GetTranslateData(r.Context(), pipeW, offset)

	if err != nil {
		if errors.Cause(err) == pilosa.ErrNotImplemented {
			http.Error(w, err.Error(), http.StatusNotImplemented)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// Flush header so client can continue.
	w.WriteHeader(http.StatusOK)
	if w, ok := w.(http.Flusher); ok {
		w.Flush()
	}

	io.Copy(w, pipeR)
}

type queryValidationSpec struct {
	required []string
	args     map[string]struct{}
}

func queryValidationSpecRequired(requiredArgs ...string) *queryValidationSpec {
	args := map[string]struct{}{}
	for _, arg := range requiredArgs {
		args[arg] = struct{}{}
	}

	return &queryValidationSpec{
		required: requiredArgs,
		args:     args,
	}
}

func (s *queryValidationSpec) Optional(args ...string) *queryValidationSpec {
	for _, arg := range args {
		s.args[arg] = struct{}{}
	}
	return s
}

func (s queryValidationSpec) validate(query url.Values) error {
	for _, req := range s.required {
		if query.Get(req) == "" {
			return errors.Errorf("%s is required", req)
		}
	}
	for k := range query {
		if _, ok := s.args[k]; !ok {
			return errors.Errorf("%s is not a valid argument", k)
		}
	}
	return nil
}

func GetHTTPClient(t *tls.Config) *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          1000,
		MaxIdleConnsPerHost:   200,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if t != nil {
		transport.TLSClientConfig = t
	}
	return &http.Client{Transport: transport}
}
