package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestVideoAttributes(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        float64
	}{
		{"fractional", `{"streams":[{"codec_type":"video","width":32,"height":24,"duration":"0.5"}]}`, 0.5},
		{"fallback on N/A", `{"streams":[{"codec_type":"video","width":32,"height":24,"duration":"N/A"}],"format":{"duration":"3.5"}}`, 3.5},
		{"nonfinite", `{"streams":[{"codec_type":"video","width":32,"height":24,"duration":"NaN"}],"format":{"duration":"Inf"}}`, 0},
		{"attached cover only", `{"streams":[{"codec_type":"video","width":32,"height":24,"disposition":{"attached_pic":1}}],"format":{"duration":"5"}}`, 0},
		{"cover before video", `{"streams":[{"codec_type":"video","width":200,"height":200,"disposition":{"attached_pic":1}},{"codec_type":"video","width":32,"height":24,"duration":"2"}]}`, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var result ffprobeResult
			if err := json.Unmarshal([]byte(tc.input), &result); err != nil {
				t.Fatal(err)
			}
			got := videoAttributes(result)
			if tc.want == 0 {
				if got != nil {
					t.Fatalf("expected invalid video, got %v", got)
				}
				return
			}
			if got == nil || got.Duration != tc.want || got.W != 32 || got.H != 24 {
				t.Fatalf("expected %gs, 32x24; got %v", tc.want, got)
			}
		})
	}
}

func TestCollectFilesSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "regular.txt")
	if err := os.WriteFile(file, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "symlink.txt")
	if err := os.Symlink(file, link); err != nil {
		t.Skipf("cannot create symlinks: %v", err)
	}
	got, isDir, err := collectFiles(dir)
	if err != nil || !isDir || !slices.Equal(got, []string{file}) {
		t.Fatalf("collectFiles directory = %v, %v, %v", got, isDir, err)
	}
	if _, _, err := collectFiles(link); err == nil {
		t.Fatal("explicit symlink must not be treated as uploadable")
	}
}
