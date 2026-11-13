package updater

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/smart/lightgbm"
	C "github.com/metacubex/mihomo/constant"
)

func TestLgbmUpdaterRetriesOnNextTickAfterStartupDownloadFailure(t *testing.T) {
	previousHome := C.Path.HomeDir()
	C.SetHomeDir(t.TempDir())
	t.Cleanup(func() { C.SetHomeDir(previousHome) })

	modelPath := C.Path.SmartModel()
	oldModel := `tree
version=v4
num_class=1
num_tree_per_iteration=1
max_feature_idx=0
	tree_sizes=44

Tree=0
num_leaves=1
num_cat=0
leaf_value=1

`
	newModel := strings.Replace(oldModel, "leaf_value=1", "leaf_value=2", 1)
	if err := os.WriteFile(modelPath, []byte(oldModel), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(modelPath, stale, stale); err != nil {
		t.Fatal(err)
	}

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Length", strconv.Itoa(len(newModel)))
			_, _ = io.WriteString(w, newModel[:len(newModel)-1])
			return
		}

		data, err := os.ReadFile(modelPath)
		if err != nil || string(data) != oldModel {
			t.Errorf("model at retry = %q, %v, want previous model", data, err)
		}
		_, _ = io.WriteString(w, newModel)
	}))
	defer server.Close()
	previousURL := lightgbm.LgbmUrl()
	lightgbm.SetLgbmUrl(server.URL)
	t.Cleanup(func() { lightgbm.SetLgbmUrl(previousURL) })

	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLgbmUpdater(1, ticks)
	}()
	select {
	case ticks <- time.Now():
		close(ticks)
		<-done
	case <-done:
		t.Fatal("updater stopped before the next scheduled tick")
	}

	if got := requests.Load(); got != 2 {
		t.Fatalf("got %d model requests, want one startup attempt and one scheduled retry", got)
	}
	data, err := os.ReadFile(modelPath)
	if err != nil || string(data) != newModel {
		t.Fatalf("model after retry = %q, %v, want updated model", data, err)
	}
}
