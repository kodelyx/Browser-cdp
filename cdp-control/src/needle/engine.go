// Package needle is a CGo bridge onto the native Needle 3 runtime.
//
// Needle 3 is Cactus Compute's on-device tool-calling model. The runtime ships as
// a self-contained dynamic library plus a quantised weights blob, and this
// package dlopen()s the one and needle_load()s the other so a Go program can do
// grammar-constrained tool calling and embedding with no Python, no server and
// no API key.
//
// This file is a copy of needle3-slm/engine/needle.go. It is duplicated rather
// than imported because cdp-control is a standalone repository — pointing go.mod
// at a sibling private repo would mean nobody else could build this one.
package needle

/*
#include <stdlib.h>
#include <dlfcn.h>
#include <stdint.h>

typedef int (*needle_load_fn)(const char* cact, uint64_t n);
typedef int (*needle_init_fn)(const char* system, const char* tools_json, const char* tool_index_path);
typedef int (*needle_complete_fn)(const char* prompt, int max_new_tokens, char* out_buffer, int buffer_size);
typedef int (*needle_embed_fn)(const char* prompt, float* out_buffer, int buffer_size);
typedef void (*needle_reset_fn)(void);
typedef const char* (*needle_last_error_fn)(void);

static void* load_needle_lib(const char* path) {
    return dlopen(path, RTLD_LAZY | RTLD_GLOBAL);
}

static int call_needle_load(void* fn, const char* cact, uint64_t n) {
    return ((needle_load_fn)fn)(cact, n);
}

static int call_needle_init(void* fn, const char* system, const char* tools_json, const char* tool_index_path) {
    return ((needle_init_fn)fn)(system, tools_json, tool_index_path);
}

static int call_needle_complete(void* fn, const char* prompt, int max_new_tokens, char* out_buffer, int buffer_size) {
    return ((needle_complete_fn)fn)(prompt, max_new_tokens, out_buffer, buffer_size);
}

static int call_needle_embed(void* fn, const char* prompt, float* out_buffer, int buffer_size) {
    return ((needle_embed_fn)fn)(prompt, out_buffer, buffer_size);
}

static void call_needle_reset(void* fn) {
    ((needle_reset_fn)fn)();
}

static const char* call_needle_last_error(void* fn) {
    return ((needle_last_error_fn)fn)();
}
*/
import "C"
import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"
)

const (
	NeedleEngineVersion = "3.0.1"
	NeedleRepo          = "Cactus-Compute/needle3"
	BaseWeightsName     = "needle3.cact"
)

type NeedleEngine struct {
	mu        sync.Mutex
	libHandle unsafe.Pointer
	loadFn    unsafe.Pointer
	initFn    unsafe.Pointer
	compFn    unsafe.Pointer
	embedFn   unsafe.Pointer
	resetFn   unsafe.Pointer
	errFn     unsafe.Pointer
	isLoaded  bool
}

var (
	needleMu     sync.Mutex
	globalNeedle *NeedleEngine
)

// FunctionCall is one tool call the model chose, decoded out of the runtime's
// JSON envelope.
//
// Named rather than anonymous so a caller can pass a decoded response around
// without restating the struct type at every boundary.
type FunctionCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// NeedleResponse matches Needle 3 output structure
type NeedleResponse struct {
	Type          string            `json:"type"` // "call" or "respond"
	Success       bool              `json:"success"`
	Reasoning     string            `json:"reasoning"`
	Confidence    *float64          `json:"confidence,omitempty"`
	PrefillTPS    *float64          `json:"prefill_tps,omitempty"`
	DecodeTPS     *float64          `json:"decode_tps,omitempty"`
	PeakRAMMB     *float64          `json:"peak_ram_mb,omitempty"`
	Validation    *NeedleValidation `json:"validation,omitempty"`
	FunctionCalls []FunctionCall    `json:"function_calls"`
	Error         any               `json:"error,omitempty"`
	ErrorCode     any               `json:"error_code,omitempty"`
}

type NeedleValidation struct {
	Ungrounded []string `json:"ungrounded"`
	Negation   bool     `json:"negation"`
}

func getLibFileName() string {
	switch runtime.GOOS {
	case "darwin":
		return "libneedle3.dylib"
	case "windows":
		return "needle3.dll"
	default:
		return "libneedle3.so"
	}
}

func getPlatformWheelName() string {
	arch := runtime.GOARCH
	switch runtime.GOOS {
	case "darwin":
		if arch == "arm64" {
			return fmt.Sprintf("cactus_needle-%s-py3-none-macosx_11_0_arm64.whl", NeedleEngineVersion)
		}
		return fmt.Sprintf("cactus_needle-%s-py3-none-macosx_11_0_x86_64.whl", NeedleEngineVersion)
	case "linux":
		if arch == "arm64" {
			return fmt.Sprintf("cactus_needle-%s-py3-none-manylinux2014_aarch64.whl", NeedleEngineVersion)
		}
		return fmt.Sprintf("cactus_needle-%s-py3-none-manylinux2014_x86_64.whl", NeedleEngineVersion)
	case "windows":
		if arch == "arm64" {
			return fmt.Sprintf("cactus_needle-%s-py3-none-win_arm64.whl", NeedleEngineVersion)
		}
		return fmt.Sprintf("cactus_needle-%s-py3-none-win_amd64.whl", NeedleEngineVersion)
	default:
		return fmt.Sprintf("cactus_needle-%s-py3-none-macosx_11_0_arm64.whl", NeedleEngineVersion)
	}
}

// EnsureNeedle3Assets ensures native library and needle3.cact exist in user cache
func EnsureNeedle3Assets() (string, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	cacheDir := filepath.Join(home, ".cache", "cactus-needle", "v3", NeedleEngineVersion)
	libPath := filepath.Join(cacheDir, getLibFileName())
	weightsPath := filepath.Join(cacheDir, BaseWeightsName)

	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", "", err
	}

	// 1. Download & Extract Native Dynamic Library if needed
	if _, err := os.Stat(libPath); os.IsNotExist(err) {
		wheelName := getPlatformWheelName()
		wheelURL := fmt.Sprintf("https://huggingface.co/%s/resolve/main/python/%s", NeedleRepo, wheelName)
		fmt.Printf("📥 Downloading Needle 3 library wheel (%s)...\n", wheelName)

		req, err := http.NewRequest("GET", wheelURL, nil)
		if err != nil {
			return "", "", fmt.Errorf("create wheel request failed: %w", err)
		}
		req.Header.Set("User-Agent", "Needle3-Go-Client/3.0.1")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", "", fmt.Errorf("failed to download wheel: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return "", "", fmt.Errorf("bad status downloading wheel: %s", resp.Status)
		}

		wheelBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", "", fmt.Errorf("read wheel body failed: %w", err)
		}

		zipReader, err := zip.NewReader(bytes.NewReader(wheelBytes), int64(len(wheelBytes)))
		if err != nil {
			return "", "", fmt.Errorf("failed to parse wheel zip: %w", err)
		}

		var foundLib io.ReadCloser
		targetMember := "needle/" + getLibFileName()
		for _, f := range zipReader.File {
			if f.Name == targetMember || filepath.Base(f.Name) == getLibFileName() {
				foundLib, err = f.Open()
				if err != nil {
					return "", "", err
				}
				break
			}
		}

		if foundLib == nil {
			return "", "", fmt.Errorf("target library %s not found in wheel", targetMember)
		}
		defer foundLib.Close()

		outLib, err := os.Create(libPath)
		if err != nil {
			return "", "", err
		}
		if _, err := io.Copy(outLib, foundLib); err != nil {
			outLib.Close()
			return "", "", err
		}
		outLib.Close()
		fmt.Printf("✅ Needle 3 library extracted to %s\n", libPath)
	}

	// 2. Download Base Weights (needle3.cact) if needed
	if _, err := os.Stat(weightsPath); os.IsNotExist(err) {
		weightsURL := fmt.Sprintf("https://huggingface.co/%s/resolve/main/%s", NeedleRepo, BaseWeightsName)
		fmt.Printf("📥 Downloading Needle 3 weights (%s, ~35MB)...\n", BaseWeightsName)

		req, err := http.NewRequest("GET", weightsURL, nil)
		if err != nil {
			return "", "", fmt.Errorf("create weights request failed: %w", err)
		}
		req.Header.Set("User-Agent", "Needle3-Go-Client/3.0.1")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", "", fmt.Errorf("failed to download weights: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return "", "", fmt.Errorf("bad status downloading weights: %s", resp.Status)
		}

		outWeights, err := os.Create(weightsPath)
		if err != nil {
			return "", "", err
		}
		if _, err := io.Copy(outWeights, resp.Body); err != nil {
			outWeights.Close()
			return "", "", err
		}
		outWeights.Close()
		fmt.Printf("✅ Needle 3 weights saved to %s\n", weightsPath)
	}

	return libPath, weightsPath, nil
}

// GetNeedleEngine returns the process-wide Needle 3 engine, loading it on first
// use.
//
// A failed load is deliberately *not* remembered. The first call downloads the
// native library and a ~35 MB weights blob; caching that failure would leave a
// long-running server permanently broken after one network hiccup, and the second
// request deserves to try rather than inherit the first one's bad luck.
//
// The runtime itself is one process-global, non-thread-safe model, so the lock is
// held across the whole load: concurrent first callers wait for one download
// instead of racing on ten.
func GetNeedleEngine() (*NeedleEngine, error) {
	needleMu.Lock()
	defer needleMu.Unlock()

	if globalNeedle != nil {
		return globalNeedle, nil
	}

	engine, err := loadNeedleEngine()
	if err != nil {
		return nil, err
	}
	globalNeedle = engine
	return globalNeedle, nil
}

// loadNeedleEngine locates the assets, dlopen()s the runtime and loads the
// weights. Callers must hold needleMu.
func loadNeedleEngine() (*NeedleEngine, error) {
	engine := &NeedleEngine{}

	libPath, weightsPath, err := EnsureNeedle3Assets()
	if err != nil {
		return nil, fmt.Errorf("could not locate needle 3 assets: %w", err)
	}

	cPath := C.CString(libPath)
	defer C.free(unsafe.Pointer(cPath))

	handle := C.load_needle_lib(cPath)
	if handle == nil {
		return nil, fmt.Errorf("dlopen failed on %s: %s", libPath, C.GoString(C.dlerror()))
	}

	loadPtr := C.dlsym(handle, C.CString("needle_load"))
	initPtr := C.dlsym(handle, C.CString("needle_init"))
	compPtr := C.dlsym(handle, C.CString("needle_complete"))
	embedPtr := C.dlsym(handle, C.CString("needle_embed"))
	resetPtr := C.dlsym(handle, C.CString("needle_reset"))
	errPtr := C.dlsym(handle, C.CString("needle_last_error"))

	if loadPtr == nil || initPtr == nil || compPtr == nil {
		return nil, fmt.Errorf("failed to load dlsym needle symbols from %s", libPath)
	}

	// Read and load needle3.cact weights into memory
	weightsBytes, rErr := os.ReadFile(weightsPath)
	if rErr != nil {
		return nil, fmt.Errorf("failed to read needle3 weights from %s: %w", weightsPath, rErr)
	}

	rcLoad := C.call_needle_load(loadPtr, (*C.char)(unsafe.Pointer(&weightsBytes[0])), C.uint64_t(len(weightsBytes)))
	if int(rcLoad) < 0 {
		var lastErr string
		if errPtr != nil {
			lastErr = C.GoString(C.call_needle_last_error(errPtr))
		}
		return nil, fmt.Errorf("needle_load failed (code %d): %s", int(rcLoad), lastErr)
	}

	engine.libHandle = handle
	engine.loadFn = loadPtr
	engine.initFn = initPtr
	engine.compFn = compPtr
	engine.embedFn = embedPtr
	engine.resetFn = resetPtr
	engine.errFn = errPtr
	engine.isLoaded = true

	return engine, nil
}

// CompleteTools passes the prompt and OpenAI tools JSON to Needle 3 C++ engine
func (n *NeedleEngine) CompleteTools(prompt string, toolsJSON string) (*NeedleResponse, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	cSystem := C.CString("You are a helpful assistant with access to tools.")
	defer C.free(unsafe.Pointer(cSystem))

	cTools := C.CString(toolsJSON)
	defer C.free(unsafe.Pointer(cTools))

	cEmpty := C.CString("")
	defer C.free(unsafe.Pointer(cEmpty))

	initRes := C.call_needle_init(n.initFn, cSystem, cTools, cEmpty)
	if int(initRes) < 0 {
		var lastErr string
		if n.errFn != nil {
			lastErr = C.GoString(C.call_needle_last_error(n.errFn))
		}
		return nil, fmt.Errorf("needle_init failed with code %d: %s", int(initRes), lastErr)
	}

	cPrompt := C.CString(prompt)
	defer C.free(unsafe.Pointer(cPrompt))

	bufSize := 16384
	outBuf := make([]byte, bufSize)

	resLen := C.call_needle_complete(n.compFn, cPrompt, C.int(256), (*C.char)(unsafe.Pointer(&outBuf[0])), C.int(bufSize))
	if int(resLen) < 0 {
		var lastErr string
		if n.errFn != nil {
			lastErr = C.GoString(C.call_needle_last_error(n.errFn))
		}
		return nil, fmt.Errorf("needle_complete failed with code %d: %s", int(resLen), lastErr)
	}

	outputStr := C.GoString((*C.char)(unsafe.Pointer(&outBuf[0])))

	var needleResp NeedleResponse
	if err := json.Unmarshal([]byte(outputStr), &needleResp); err != nil {
		return nil, fmt.Errorf("failed to parse needle 3 json (%w): %s", err, outputStr)
	}

	return &needleResp, nil
}

// Embed generates text embeddings using Needle 3
func (n *NeedleEngine) Embed(text string) ([]float32, error) {
	if n.embedFn == nil {
		return nil, fmt.Errorf("needle_embed symbol not available")
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	cText := C.CString(text)
	defer C.free(unsafe.Pointer(cText))

	// Calling with null output returns embedding dimension
	dim := C.call_needle_embed(n.embedFn, cText, nil, 0)
	if int(dim) <= 0 {
		return nil, fmt.Errorf("needle_embed dimension query failed (code %d)", int(dim))
	}

	outBuf := make([]float32, int(dim))
	rc := C.call_needle_embed(n.embedFn, cText, (*C.float)(unsafe.Pointer(&outBuf[0])), dim)
	if rc != dim {
		return nil, fmt.Errorf("needle_embed computation failed (code %d)", int(rc))
	}

	return outBuf, nil
}

// Reset resets the engine state
func (n *NeedleEngine) Reset() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.resetFn != nil {
		C.call_needle_reset(n.resetFn)
	}
}
