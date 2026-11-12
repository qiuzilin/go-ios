package tunnel

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ed25519"
	"howett.net/plist"
)

func TestPairRecordManager(t *testing.T) {
	tmp := t.TempDir()

	pm, err := NewPairRecordManager(tmp)

	require.NoError(t, err)

	t.Run("self identity is created", func(t *testing.T) {
		_, err := os.Stat(path.Join(tmp, "selfIdentity.plist"))
		assert.NoError(t, err)
	})
	t.Run("read key equals the stored one", func(t *testing.T) {
		siPath := path.Join(tmp, "selfIdentity.plist")
		b, err := os.ReadFile(siPath)
		require.NoError(t, err)

		var si selfIdentity
		_, err = plist.Unmarshal(b, &si)
		require.NoError(t, err)

		private := ed25519.NewKeyFromSeed(si.PrivateKey)

		assert.True(t, private.Equal(pm.selfId.privateKey()))
	})
}

func TestPairIdentityProcess(t *testing.T) {
	dir := os.Getenv("GO_IOS_PAIR_TEST_DIR")
	if dir == "" {
		return
	}
	pm, err := NewPairRecordManager(dir)
	require.NoError(t, err)
	fmt.Println(pm.selfId.Identifier)
}

func TestPairIdentityConcurrentProcesses(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	outputs := make(chan string, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestPairIdentityProcess$")
			cmd.Env = append(os.Environ(), "GO_IOS_PAIR_TEST_DIR="+dir)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("concurrent identity initialization: %v: %s", err, out)
				return
			}
			outputs <- strings.Split(strings.TrimSpace(string(out)), "\n")[0]
		}()
	}
	wg.Wait()
	close(outputs)
	pm, err := NewPairRecordManager(dir)
	require.NoError(t, err)
	for id := range outputs {
		require.Equal(t, pm.selfId.Identifier, id)
	}
}

func TestPairDeviceWritesAreAtomic(t *testing.T) {
	dir := t.TempDir()
	pm, err := NewPairRecordManager(dir)
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(path.Join(dir, "peers"), 0o700))
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				err := pm.StoreDeviceInfo(device{Identifier: "device", Name: fmt.Sprint(i), Info: make([]byte, 1024*(i+1))})
				if err != nil {
					t.Error(err)
				}
				data, err := os.ReadFile(path.Join(dir, "peers", "device.plist"))
				if err == nil {
					var got device
					_, err = plist.Unmarshal(data, &got)
				}
				if err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}
