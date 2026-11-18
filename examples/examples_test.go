// Package examples_test runs the two example programs against local stand-ins, so the examples are
// known to work without calling the real API.
package examples_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qubitoz/grualink-api-go/grualink"
)

// Obviously fake values.
const (
	fakeKey    = "glk_test_FAKE_KEY_FOR_EXAMPLES_ONLY"
	fakeSecret = "whsec_TEST_ONLY_FAKE_SECRET_0001"
)

// build compiles one example into a temporary folder and returns the path of the binary.
func build(t *testing.T, name string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), name)
	output, err := exec.Command("go", "build", "-o", binary, "./"+name).CombinedOutput()
	if err != nil {
		t.Fatalf("go build ./%s: %v\n%s", name, err, output)
	}
	return binary
}

// environment keeps only what a program needs to start, plus the given variables.
func environment(variables ...string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	return append(env, variables...)
}

// fakeAPI behaves like the API for the calls of the request example, idempotency included.
type fakeAPI struct {
	mu       sync.Mutex
	calls    []string
	created  map[string]storedRequest
	failures []string
}

type storedRequest struct {
	body   []byte
	object map[string]any
}

func (f *fakeAPI) note(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeAPI) fail(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, fmt.Sprintf(format, args...))
}

// seen returns the calls received so far, in order, and the problems noticed.
func (f *fakeAPI) seen() (calls string, failures []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, " "), append([]string(nil), f.failures...)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": "Mensaje de ejemplo."}})
}

func (f *fakeAPI) authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+fakeKey {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func (f *fakeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		f.note("health")
		if r.Header.Get("Authorization") != "" {
			f.fail("health was sent with the key")
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": "v1"})
	})
	mux.HandleFunc("POST /api/v1/service-requests", func(w http.ResponseWriter, r *http.Request) {
		if !f.authorized(w, r) {
			return
		}
		key := r.Header.Get("Idempotency-Key")
		body, _ := io.ReadAll(r.Body)
		var input grualink.ServiceRequestInput
		if key == "" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") || json.Unmarshal(body, &input) != nil {
			f.fail("malformed create request")
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if stored, found := f.created[key]; found {
			if !bytes.Equal(stored.body, body) {
				f.calls = append(f.calls, "conflict")
				writeError(w, http.StatusConflict, "idempotency_conflict")
				return
			}
			f.calls = append(f.calls, "replay")
			w.Header().Set("Idempotent-Replayed", "true")
			writeJSON(w, http.StatusOK, stored.object)
			return
		}
		f.calls = append(f.calls, "create")
		object := map[string]any{
			"id":               fmt.Sprintf("11111111-2222-4333-8444-%012d", len(f.created)+1),
			"object":           "service_request",
			"status":           "quoted",
			"mode":             "test",
			"external_ref":     input.ExternalRef,
			"created_at":       "2026-11-18T10:00:00.000Z",
			"resolved_at":      nil,
			"rejection_reason": nil,
			"service":          nil,
			"quote": map[string]any{
				"estimated_price": map[string]any{
					"currency": "MXN", "subtotal": 864.33,
					"taxes":     []any{map[string]any{"name": "IVA", "type": "tax", "rate": 16, "amount": 138.29}},
					"tax_total": 138.29, "retention_total": 0, "total": 1002.62,
				},
				"includes_extras": false, "approximate_distance": true, "unpriced_reason": nil,
				"note": "Precio estimado.",
			},
			"assignment": nil,
			"timeline":   nil,
		}
		f.created[key] = storedRequest{body: body, object: object}
		writeJSON(w, http.StatusCreated, object)
	})
	mux.HandleFunc("GET /api/v1/service-requests/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !f.authorized(w, r) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, "get")
		for _, stored := range f.created {
			if stored.object["id"] == r.PathValue("id") {
				// With a test key, the prefix of external_ref decides the final status.
				current := map[string]any{}
				for name, value := range stored.object {
					current[name] = value
				}
				if strings.HasPrefix(fmt.Sprint(current["external_ref"]), "TEST-CONFIRMAR") {
					current["status"] = "confirmed"
				}
				writeJSON(w, http.StatusOK, current)
				return
			}
		}
		writeError(w, http.StatusNotFound, "not_found")
	})
	mux.HandleFunc("GET /api/v1/service-requests", func(w http.ResponseWriter, r *http.Request) {
		if !f.authorized(w, r) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, "find")
		data := []any{}
		for _, stored := range f.created {
			if stored.object["external_ref"] == r.URL.Query().Get("external_ref") {
				data = append(data, stored.object)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
	})
	return mux
}

func TestRequestTowExample(t *testing.T) {
	binary := build(t, "request-tow")
	api := &fakeAPI{created: map[string]storedRequest{}}
	server := httptest.NewServer(api.handler())
	defer server.Close()

	run := func(variables ...string) (string, error) {
		command := exec.Command(binary)
		command.Env = environment(variables...)
		output, err := command.CombinedOutput()
		return string(output), err
	}

	output, err := run("GRUALINK_API_KEY="+fakeKey, "GRUALINK_API_BASE_URL="+server.URL+"/api/v1")
	t.Logf("output of the example:\n%s", output)
	if err != nil {
		t.Fatalf("the example failed: %v", err)
	}
	for _, expected := range []string{
		"API status: ok (v1)",
		`with status "quoted", reference TEST-CONFIRMAR-`,
		"Subtotal:  864.33 MXN",
		"+ IVA (16%): 138.29",
		"Total:     1002.62 MXN",
		"replayed=true, same id=true",
		`Status now: "confirmed"`,
		"Requests with reference TEST-CONFIRMAR-",
		": 1\n",
	} {
		if !strings.Contains(output, expected) {
			t.Errorf("the output lacks %q", expected)
		}
	}
	if strings.Contains(output, fakeKey) {
		t.Errorf("the example printed the key")
	}
	calls, failures := api.seen()
	if want := "health create replay get find"; calls != want {
		t.Errorf("calls = %q, want %q", calls, want)
	}
	if len(failures) != 0 {
		t.Errorf("the fake API saw: %v", failures)
	}

	// It refuses to run without a key or with a live key, before sending anything.
	refusals := map[string][]string{
		"no key":   {"GRUALINK_API_BASE_URL=" + server.URL + "/api/v1"},
		"live key": {"GRUALINK_API_KEY=glk_live_FAKE_KEY_FOR_EXAMPLES_ONLY", "GRUALINK_API_BASE_URL=" + server.URL + "/api/v1"},
	}
	for name, variables := range refusals {
		output, err := run(variables...)
		if err == nil {
			t.Errorf("%s: the example must exit with an error; output:\n%s", name, output)
		}
		if strings.Contains(output, "FAKE_KEY") {
			t.Errorf("%s: the example printed the key", name)
		}
	}
	if after, _ := api.seen(); after != calls {
		t.Errorf("a refused run reached the API: %q", after)
	}

	// An API error is reported in one line and with a failing exit code.
	output, err = run("GRUALINK_API_KEY=glk_test_ANOTHER_FAKE_KEY", "GRUALINK_API_BASE_URL="+server.URL+"/api/v1")
	if err == nil || !strings.Contains(output, "API error 401 unauthorized") {
		t.Errorf("expected a 401 to be reported, got (%v):\n%s", err, output)
	}
}

// lockedBuffer collects the output of a running program.
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestWebhookServerExample(t *testing.T) {
	binary := build(t, "webhook-server")

	// Without a secret it refuses to start.
	refused := exec.Command(binary)
	refused.Env = environment()
	if output, err := refused.CombinedOutput(); err == nil || !strings.Contains(string(output), "GRUALINK_WEBHOOK_SECRET") {
		t.Fatalf("expected a refusal without a secret, got (%v):\n%s", err, output)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	var output lockedBuffer
	command := exec.Command(binary)
	command.Env = environment("GRUALINK_WEBHOOK_SECRET="+fakeSecret, fmt.Sprintf("PORT=%d", port))
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Logf("output of the example:\n%s", output.String())
	}()

	url := fmt.Sprintf("http://127.0.0.1:%d/webhooks/grualink", port)
	eventually(t, "the example to listen", func() bool {
		connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err != nil {
			return false
		}
		connection.Close()
		return true
	})

	post := func(body string, id, signature string) (int, string) {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json; charset=utf-8")
		request.Header.Set("User-Agent", "GRUALINK-Avisos/1.0")
		request.Header.Set("X-GruaLink-Event", "service_request.confirmed")
		request.Header.Set("X-GruaLink-Delivery", id)
		request.Header.Set("X-GruaLink-Mode", "test")
		if signature != "" {
			request.Header.Set("X-GruaLink-Signature", signature)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		answer, _ := io.ReadAll(response.Body)
		return response.StatusCode, strings.TrimSpace(string(answer))
	}

	id := "aaaaaaaa-0000-4000-8000-000000000001"
	body := `{"id": "` + id + `", "data": {"previous_status": "quoted", "service_request": {"id": "11111111-2222-4333-8444-555555555555", "status": "confirmed", "external_ref": "TEST-CONFIRMAR-0001"}}, "mode": "test", "type": "service_request.confirmed", "object": "event", "created_at": "2026-11-18T10:05:01.000Z"}`
	signature := grualink.BuildSignatureHeader([]string{fakeSecret}, time.Now().Unix(), []byte(body))

	if status, answer := post(body, id, signature); status != 200 || answer != `{"received":true}` {
		t.Errorf("signed delivery: %d %s", status, answer)
	}
	if status, answer := post(body, id, signature); status != 200 || answer != `{"received":true,"duplicate":true}` {
		t.Errorf("repeated delivery: %d %s", status, answer)
	}
	if status, answer := post(strings.Replace(body, "confirmed", "completed", 1), id, signature); status != 401 || answer != `{"received":false,"error":"invalid_signature"}` {
		t.Errorf("tampered delivery: %d %s", status, answer)
	}
	if status, _ := post(body, id, ""); status != 401 {
		t.Errorf("unsigned delivery: %d", status)
	}
	stale := grualink.BuildSignatureHeader([]string{fakeSecret}, time.Now().Unix()-3600, []byte(body))
	if status, _ := post(body, id, stale); status != 401 {
		t.Errorf("stale delivery: %d", status)
	}

	eventually(t, "the event to be printed", func() bool {
		return strings.Contains(output.String(), `service_request.confirmed: request 11111111-2222-4333-8444-555555555555 (TEST-CONFIRMAR-0001) is now "confirmed"`)
	})
	// The reasons stay in the log of the receiver.
	for _, reason := range []string{"(duplicate)", "(signature_mismatch)", "(missing_header)", "(timestamp_out_of_tolerance)"} {
		eventually(t, "the log to have "+reason, func() bool { return strings.Contains(output.String(), reason) })
	}
	printed := output.String()
	if strings.Count(printed, "is now") != 1 {
		t.Errorf("the event must be processed once")
	}
	if strings.Contains(printed, fakeSecret) {
		t.Errorf("the example printed the secret")
	}
}
