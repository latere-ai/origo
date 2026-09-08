// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package lfs serves the Git LFS batch API of spec 010. Object bytes
// never pass through a node: the batch answers presigned URLs on the
// bucket, signed against ORIGO_S3_PUBLIC_ENDPOINT, and the client's
// transfer goes to the store directly. An upload is complete when
// verify has checked the stored object's size and written the marker
// lfs/verified/<oid>, which is what a download action requires.
//
// The batch API carries its own error shape, so the envelope of spec
// 003 is not used on these paths; the sentences are the same code
// table's, rendered through contract.Sentence.
package lfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"latere.ai/x/pkg/s3"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/tracing"
	"github.com/latere-ai/origo/internal/wal"
)

// The values spec 010 fixes.
const (
	// MediaType is the content type of every request and response of the
	// batch API.
	MediaType = "application/vnd.git-lfs+json"
	// MaxBatchBytes bounds a batch or verify request body (spec 012).
	MaxBatchBytes = 1 << 20
	// PresignTTL is how long a transfer URL is valid.
	PresignTTL = 15 * time.Minute
	// ObjectContentType is the type an upload is pinned to: the presigned
	// PUT signs it, so an upload of another type is refused by the store.
	ObjectContentType = "application/octet-stream"
	// DocumentationURL is the documentation_url of every LFS error body.
	DocumentationURL = "https://github.com/latere-ai/origo/blob/main/specs/010-lfs.md"
	// BasicTransfer is the one transfer adapter this spec serves.
	BasicTransfer = "basic"
	// listPage is the page size of the lfs/ listing the quota rule sums.
	listPage = 1000
)

// Presigner signs the transfer URLs. It is pkg/s3's client, built
// against the public endpoint; the interface is here so a test signs
// against its own endpoint without a node.
type Presigner interface {
	PresignGet(key string, expires time.Duration) (string, error)
	PresignPut(key string, expires time.Duration, contentLength int64, opts ...s3.PresignOption) (string, error)
}

// Options configures the handler.
type Options struct {
	// Log resolves names and reads the newest index; required.
	Log *wal.Log
	// Guard decides every request before the repository is looked up;
	// required.
	Guard *auth.Guard
	// Presigner signs the transfer URLs; required.
	Presigner Presigner
	Logger    *slog.Logger
	// Now is the clock the verified marker is stamped with.
	Now func() time.Time
}

// Handler serves the LFS routes.
type Handler struct {
	log       *wal.Log
	store     wal.Store
	guard     *auth.Guard
	presigner Presigner
	logger    *slog.Logger
	now       func() time.Time
}

// New builds the handler.
func New(o Options) *Handler {
	if o.Log == nil || o.Guard == nil || o.Presigner == nil {
		panic("lfs: the handler needs a log, a guard, and a presigner")
	}
	h := &Handler{log: o.Log, store: o.Log.Store(), guard: o.Guard, presigner: o.Presigner, logger: o.Logger, now: o.Now}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	if h.now == nil {
		h.now = time.Now
	}
	return h
}

// Register mounts the routes in both URL forms spec 003 names, the id
// form /r/{id}.git and the label form /{owner}/{slug}.git. Each is more
// specific than the smart HTTP wildcard /{owner}/{slug}/{service...} of
// internal/httpgit, which is what keeps the two apart in one mux. Every
// path under locks is mounted with no method, because the answer is the
// same whatever the method.
func (h *Handler) Register(mux *http.ServeMux) {
	for _, repo := range []string{"/r/{id}", "/{owner}/{slug}"} {
		mux.HandleFunc("POST "+repo+"/info/lfs/objects/batch", h.batch)
		mux.HandleFunc("POST "+repo+"/info/lfs/verify", h.verify)
		mux.HandleFunc(repo+"/info/lfs/locks", h.locks)
		mux.HandleFunc(repo+"/info/lfs/locks/{rest...}", h.locks)
	}
}

// batchRequest is the body of the batch API.
type batchRequest struct {
	Operation string          `json:"operation"`
	Objects   []requestObject `json:"objects"`
	Transfers []string        `json:"transfers"`
	Ref       *struct {
		Name string `json:"name"`
	} `json:"ref"`
	HashAlgo string `json:"hash_algo"`
}

type requestObject struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

type batchResponse struct {
	Transfer string           `json:"transfer"`
	Objects  []responseObject `json:"objects"`
}

type responseObject struct {
	OID     string            `json:"oid"`
	Size    int64             `json:"size"`
	Actions map[string]action `json:"actions,omitempty"`
	Error   *objectError      `json:"error,omitempty"`
}

// action is one transfer the client performs itself.
type action struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header,omitempty"`
	ExpiresIn int               `json:"expires_in,omitempty"`
}

// objectError is a per-object failure: the status as the code, and the
// sentence of the code table. The LFS shape carries no details.
type objectError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// errorBody is a top-level failure: the sentence of the code table, the
// request's trace id, and this spec's URL. There is no details object
// in the LFS shape, so the developer fields of spec 003 go to the log.
type errorBody struct {
	Message          string `json:"message"`
	RequestID        string `json:"request_id"`
	DocumentationURL string `json:"documentation_url"`
}

// fail answers a top-level failure in the LFS shape.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, status int, code string) {
	write(w, status, errorBody{
		Message:          contract.Sentence(code),
		RequestID:        requestID(r),
		DocumentationURL: DocumentationURL,
	})
}

// requestID is the trace id of the request's span, so the LFS body and
// the request log line name one request (spec 011). Without an exporter
// configured, or on a request the sampler dropped, there is no span and
// the id is a fresh UUID, which is what a client quoting it to an
// operator needs either way.
func requestID(r *http.Request) string {
	if id := tracing.ID(r.Context()); id != "" {
		return id
	}
	return uuid.NewString()
}

func write(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		// Every body this package sends is a struct of strings, numbers,
		// and maps of them. An empty object keeps a client that somehow
		// reaches this on the LFS shape rather than on no body at all.
		raw = []byte("{}")
	}
	w.Header().Set("Content-Type", MediaType)
	w.WriteHeader(status)
	_, _ = w.Write(append(raw, '\n'))
}

// locks answers every path under locks, whatever the method: 501 with
// the LFS body. A client that reads it turns locking off and goes on.
func (h *Handler) locks(w http.ResponseWriter, r *http.Request) {
	h.fail(w, r, http.StatusNotImplemented, contract.CodeLFSLocksUnsupported)
}

// resolve maps the request path to a repository and asks the guard for
// the action before anything of the repository is read (spec 007,
// authorization before lookup), the way internal/httpgit does, and
// returns the decision so the quota rule reads quota_bytes from it. A
// refusal is written in the LFS shape here rather than by the guard,
// because the guard writes spec 003's envelope.
func (h *Handler) resolve(w http.ResponseWriter, r *http.Request, act auth.Action) (string, auth.Decision, bool) {
	var ref auth.RepoRef
	if id := r.PathValue("id"); id != "" {
		ref.ID = strings.TrimSuffix(id, ".git")
	} else {
		ref.Owner, ref.Slug = r.PathValue("owner"), strings.TrimSuffix(r.PathValue("slug"), ".git")
		id, err := h.log.Resolve(r.Context(), ref.Owner, ref.Slug)
		if err != nil && !errors.Is(err, wal.ErrNotFound) {
			h.storageError(w, r, "resolve", err)
			return "", auth.Decision{}, false
		}
		ref.ID = id
	}
	d, err := h.guard.Decide(r.Context(), auth.FromContext(r.Context()), ref, act)
	if err != nil {
		h.refuse(w, r, err)
		return "", auth.Decision{}, false
	}
	if ref.ID == "" {
		h.fail(w, r, http.StatusNotFound, contract.CodeRepoNotFound)
		return "", auth.Decision{}, false
	}
	return ref.ID, d, true
}

// refuse renders a *Denied or an *Unavailable in the LFS shape.
func (h *Handler) refuse(w http.ResponseWriter, r *http.Request, err error) {
	if denied, ok := errors.AsType[*auth.Denied](err); ok {
		h.logger.InfoContext(r.Context(), "lfs denied", "path", r.URL.Path, "action", string(denied.Action), "subject", denied.Subject, "reason", denied.Reason)
		h.fail(w, r, http.StatusForbidden, contract.CodeForbidden)
		return
	}
	h.logger.ErrorContext(r.Context(), "authorizer unavailable", "path", r.URL.Path, "error", err)
	h.fail(w, r, http.StatusServiceUnavailable, contract.CodeAuthorizerUnavailable)
}

func (h *Handler) storageError(w http.ResponseWriter, r *http.Request, op string, err error) {
	h.logger.ErrorContext(r.Context(), "lfs storage operation failed", "path", r.URL.Path, "op", op, "error", err)
	h.fail(w, r, http.StatusServiceUnavailable, contract.CodeStorageUnavailable)
}

// exists reports whether the repository is present and not deleted, and
// the bytes its log holds. It runs after the guard, never before.
func (h *Handler) exists(w http.ResponseWriter, r *http.Request, id string) (int64, bool) {
	ix, _, err := h.log.Newest(r.Context(), id, 0, false)
	if err != nil {
		if errors.Is(err, wal.ErrNotFound) {
			h.fail(w, r, http.StatusNotFound, contract.CodeRepoNotFound)
		} else {
			h.storageError(w, r, "newest", err)
		}
		return 0, false
	}
	if ix.DeletedAt != nil {
		h.fail(w, r, http.StatusNotFound, contract.CodeRepoNotFound)
		return 0, false
	}
	return ix.SizeBytes, true
}

// decode reads a body of at most MaxBatchBytes into v. A larger body,
// or one that does not parse, is the invalid_request sentence.
func decode(r *http.Request, v any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxBatchBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > MaxBatchBytes {
		return errors.New("body over 1 MiB")
	}
	return json.Unmarshal(raw, v)
}

// validOID is the object id rule: 64 lower-case hexadecimal characters,
// the SHA-256 the LFS pointer names. The oid becomes a key under the
// repository's prefix, so nothing else is accepted.
func validOID(oid string) bool {
	if len(oid) != 64 {
		return false
	}
	for i := range len(oid) {
		c := oid[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// objectKey and markerKey are the two keys of one object (spec 001).
func (h *Handler) objectKey(repo, oid string) string {
	return h.log.RepoPrefix(repo) + "lfs/" + oid
}

func (h *Handler) markerKey(repo, oid string) string {
	return h.log.RepoPrefix(repo) + "lfs/verified/" + oid
}

// marker is the body of lfs/verified/<oid>.
type marker struct {
	Size    int64     `json:"size"`
	At      time.Time `json:"at"`
	Subject string    `json:"subject"`
}

// batch answers each object: a presigned GET for a download, a
// presigned PUT and the verify endpoint for an upload, and no actions
// for an upload of an object the store already holds.
func (h *Handler) batch(w http.ResponseWriter, r *http.Request) {
	var req batchRequest
	if err := decode(r, &req); err != nil {
		h.logger.InfoContext(r.Context(), "lfs batch refused", "path", r.URL.Path, "reason", err.Error())
		h.fail(w, r, http.StatusBadRequest, contract.CodeInvalid)
		return
	}
	act := auth.ActionRead
	switch req.Operation {
	case "download":
	case "upload":
		act = auth.ActionWrite
	default:
		h.logger.InfoContext(r.Context(), "lfs batch refused", "path", r.URL.Path, "reason", "operation must be download or upload", "operation", req.Operation)
		h.fail(w, r, http.StatusBadRequest, contract.CodeInvalid)
		return
	}
	// An empty transfers list means basic; any list must name it,
	// because it is the one adapter this spec serves.
	if len(req.Transfers) > 0 && !containsBasic(req.Transfers) {
		h.logger.InfoContext(r.Context(), "lfs batch refused", "path", r.URL.Path, "reason", "no transfer adapter in common", "transfers", strings.Join(req.Transfers, ","))
		h.fail(w, r, http.StatusBadRequest, contract.CodeInvalid)
		return
	}
	id, decision, ok := h.resolve(w, r, act)
	if !ok {
		return
	}
	size, ok := h.exists(w, r, id)
	if !ok {
		return
	}
	var held map[string]bool
	if act == auth.ActionWrite {
		held = h.heldObjects(r, id, req.Objects)
		if !h.withinQuota(w, r, id, size, decision, req.Objects, held) {
			return
		}
	}
	out := batchResponse{Transfer: BasicTransfer, Objects: make([]responseObject, 0, len(req.Objects))}
	for _, o := range req.Objects {
		out.Objects = append(out.Objects, h.answer(w, r, id, act, o, held))
	}
	write(w, http.StatusOK, out)
}

// heldObjects is the batch API's rule for an object the server already
// has: its response object carries no actions, so the client uploads
// nothing. An object is held when lfs/verified/<oid> exists and names
// the declared size; a marker of another size is not the object the
// client has, and the upload action lets it replace the bytes. A marker
// that cannot be read is recorded as false and answered as a per-object
// 503 by answer.
func (h *Handler) heldObjects(r *http.Request, id string, objects []requestObject) map[string]bool {
	held := map[string]bool{}
	for _, o := range objects {
		if !validOID(o.OID) || o.Size < 0 {
			continue
		}
		ok, err := h.held(r, id, o)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "lfs marker read failed", "repo", id, "oid", o.OID, "error", err)
			held[o.OID] = false
			continue
		}
		if ok {
			held[o.OID] = true
		}
	}
	return held
}

// held reads the marker of one object and reports whether it names the
// declared size; a missing marker is false and no error.
func (h *Handler) held(r *http.Request, id string, o requestObject) (bool, error) {
	rc, _, err := h.store.Get(r.Context(), h.markerKey(id, o.OID), "")
	if errors.Is(err, wal.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = rc.Close() }()
	var m marker
	if err := json.NewDecoder(io.LimitReader(rc, 64<<10)).Decode(&m); err != nil {
		return false, err
	}
	return m.Size == o.Size, nil
}

func containsBasic(transfers []string) bool {
	return slices.Contains(transfers, BasicTransfer)
}

// answer builds one object of the batch response. A malformed object is
// a per-object 400; a storage failure a per-object 503, so one bad
// object does not lose the rest of the batch.
func (h *Handler) answer(w http.ResponseWriter, r *http.Request, id string, act auth.Action, o requestObject, held map[string]bool) responseObject {
	res := responseObject{OID: o.OID, Size: o.Size}
	if !validOID(o.OID) || o.Size < 0 {
		res.Error = &objectError{Code: http.StatusBadRequest, Message: contract.Sentence(contract.CodeInvalid)}
		return res
	}
	if act == auth.ActionRead {
		if _, err := h.store.Head(r.Context(), h.markerKey(id, o.OID)); err != nil {
			if !errors.Is(err, wal.ErrNotFound) {
				h.logger.ErrorContext(r.Context(), "lfs marker head failed", "repo", id, "oid", o.OID, "error", err)
				res.Error = &objectError{Code: http.StatusServiceUnavailable, Message: contract.Sentence(contract.CodeStorageUnavailable)}
				return res
			}
			res.Error = &objectError{Code: http.StatusNotFound, Message: contract.Sentence(contract.CodeLFSObjectNotStored)}
			return res
		}
		href, err := h.presigner.PresignGet(h.objectKey(id, o.OID), PresignTTL)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "lfs presign failed", "repo", id, "oid", o.OID, "error", err)
			res.Error = &objectError{Code: http.StatusServiceUnavailable, Message: contract.Sentence(contract.CodeStorageUnavailable)}
			return res
		}
		res.Actions = map[string]action{"download": {Href: href, ExpiresIn: int(PresignTTL.Seconds())}}
		return res
	}
	// An object the store holds gets no actions; a marker that could
	// not be read is a per-object 503 like any other storage failure.
	if isHeld, seen := held[o.OID]; seen {
		if isHeld {
			return res
		}
		res.Error = &objectError{Code: http.StatusServiceUnavailable, Message: contract.Sentence(contract.CodeStorageUnavailable)}
		return res
	}
	// The PUT is signed with the length and the type, so the store
	// refuses an upload of another size or type; the action names the
	// type because the basic transfer sends exactly the headers the
	// action carries and the signature covers it.
	href, err := h.presigner.PresignPut(h.objectKey(id, o.OID), PresignTTL, o.Size, s3.WithContentType(ObjectContentType))
	if err != nil {
		h.logger.ErrorContext(r.Context(), "lfs presign failed", "repo", id, "oid", o.OID, "error", err)
		res.Error = &objectError{Code: http.StatusServiceUnavailable, Message: contract.Sentence(contract.CodeStorageUnavailable)}
		return res
	}
	res.Actions = map[string]action{
		"upload": {Href: href, Header: map[string]string{"Content-Type": ObjectContentType}, ExpiresIn: int(PresignTTL.Seconds())},
		"verify": {Href: verifyHref(r), Header: verifyHeader(r), ExpiresIn: int(PresignTTL.Seconds())},
	}
	return res
}

// verifyHref is the verify endpoint of the repository the request
// names, built from the request's own origin so it is the address the
// client reached, whatever sits in front of the node.
func verifyHref(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	return scheme + "://" + r.Host + strings.TrimSuffix(r.URL.EscapedPath(), "objects/batch") + "verify"
}

// verifyHeader carries the request's own credential, so the client
// completes the upload with what it authenticated the batch with.
func verifyHeader(r *http.Request) map[string]string {
	if a := r.Header.Get("Authorization"); a != "" {
		return map[string]string{"Authorization": a}
	}
	return nil
}

// withinQuota is spec 012's repository size rule on an upload batch:
// the bytes the log holds plus the bytes under lfs/ plus the sizes of
// the objects the batch would upload, against the authorizer's
// quota_bytes. An object the store holds is in the lfs/ sum already
// and adds nothing.
func (h *Handler) withinQuota(w http.ResponseWriter, r *http.Request, id string, size int64, d auth.Decision, objects []requestObject, held map[string]bool) bool {
	stored, err := h.lfsBytes(r, id)
	if err != nil {
		h.storageError(w, r, "list", err)
		return false
	}
	total := size + stored
	for _, o := range objects {
		if o.Size > 0 && !held[o.OID] {
			total += o.Size
		}
	}
	if total <= d.QuotaBytes {
		return true
	}
	h.logger.InfoContext(r.Context(), "lfs batch over quota", "repo", id, "bytes", total, "max", d.QuotaBytes)
	h.fail(w, r, http.StatusRequestEntityTooLarge, contract.CodeOverQuota)
	return false
}

// lfsBytes sums the objects under lfs/. The delimiter groups
// lfs/verified/ into a prefix, so the markers are listed once as a
// prefix rather than one key each.
func (h *Handler) lfsBytes(r *http.Request, id string) (int64, error) {
	prefix := h.log.RepoPrefix(id) + "lfs/"
	var (
		total int64
		after string
	)
	for {
		res, err := h.store.List(r.Context(), wal.ListOptions{Prefix: prefix, StartAfter: after, Max: listPage, Delimiter: "/"})
		if err != nil {
			return 0, err
		}
		for _, o := range res.Objects {
			total += o.Size
		}
		if !res.Truncated {
			return total, nil
		}
		next := after
		if n := len(res.Objects); n > 0 {
			next = res.Objects[n-1].Key
		}
		// A page may be all prefixes; continue after the last of them,
		// the way wal.Log.Repos does.
		for _, p := range res.Prefixes {
			if p+"~" > next {
				next = p + "~"
			}
		}
		if next == after {
			return total, nil
		}
		after = next
	}
}

type verifyRequest struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

// verify completes an upload: the stored object must exist with the
// declared size, and then the marker is written. The hash is not
// checked, because computing it would read the whole object through the
// node, which the presigned transfer exists to avoid. An object whose
// bytes do not match its oid is what the client's own git lfs refuses
// on download.
func (h *Handler) verify(w http.ResponseWriter, r *http.Request) {
	var req verifyRequest
	if err := decode(r, &req); err != nil || !validOID(req.OID) || req.Size < 0 {
		reason := "oid must be 64 lower-case hexadecimal characters and size must not be negative"
		if err != nil {
			reason = err.Error()
		}
		h.logger.InfoContext(r.Context(), "lfs verify refused", "path", r.URL.Path, "reason", reason)
		h.fail(w, r, http.StatusBadRequest, contract.CodeInvalid)
		return
	}
	// verify completes an upload, so it is a write.
	id, _, ok := h.resolve(w, r, auth.ActionWrite)
	if !ok {
		return
	}
	if _, ok := h.exists(w, r, id); !ok {
		return
	}
	o, err := h.store.Head(r.Context(), h.objectKey(id, req.OID))
	switch {
	case errors.Is(err, wal.ErrNotFound):
		h.logger.InfoContext(r.Context(), "lfs verify: object not stored", "repo", id, "oid", req.OID)
		h.fail(w, r, http.StatusUnprocessableEntity, contract.CodeLFSObjectMismatch)
		return
	case err != nil:
		h.storageError(w, r, "head", err)
		return
	case o.Size != req.Size:
		h.logger.InfoContext(r.Context(), "lfs verify: size mismatch", "repo", id, "oid", req.OID, "declared", req.Size, "stored", o.Size)
		if err := h.store.Delete(r.Context(), h.objectKey(id, req.OID)); err != nil {
			h.logger.ErrorContext(r.Context(), "lfs mismatched object not deleted", "repo", id, "oid", req.OID, "error", err)
		}
		h.fail(w, r, http.StatusUnprocessableEntity, contract.CodeLFSObjectMismatch)
		return
	}
	body, err := json.Marshal(marker{Size: o.Size, At: h.now().UTC(), Subject: auth.Subject(r.Context())})
	if err != nil {
		h.storageError(w, r, "marker", err)
		return
	}
	// Create-if-absent: an existing marker is a success, because the
	// object it names is already verified.
	if _, err := h.store.Create(r.Context(), h.markerKey(id, req.OID), wal.BytesBody(body)); err != nil && !errors.Is(err, wal.ErrExists) {
		h.storageError(w, r, "create", err)
		return
	}
	write(w, http.StatusOK, struct{}{})
}

// PresignerOptions is the bucket a transfer URL is signed against: the
// same bucket and credential the log runs on, at the endpoint LFS
// clients can reach (ORIGO_S3_PUBLIC_ENDPOINT).
type PresignerOptions struct {
	Endpoint  string
	Region    string
	Bucket    string
	Key       string
	Secret    string
	PathStyle bool
}

// NewPresigner builds the signer. It opens no connection and sends
// nothing: signing is arithmetic over the request line and the
// credential.
func NewPresigner(o PresignerOptions) (Presigner, error) {
	var opts []s3.Option
	if o.PathStyle {
		opts = append(opts, s3.WithPathStyle())
	}
	c, err := s3.New(o.Endpoint, o.Region, o.Bucket, o.Key, o.Secret, opts...)
	if err != nil {
		return nil, fmt.Errorf("lfs: %w", err)
	}
	return c, nil
}
