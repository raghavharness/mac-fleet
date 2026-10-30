package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/raghavharness/mac-fleet/internal/wire"
)

// Image names are passed to the tart CLI as a single argument, never through a
// shell. The pattern still keeps option-like and odd values out.
var imageRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,255}$`)

func validImage(name string) error {
	if !imageRe.MatchString(name) {
		return fmt.Errorf("invalid image name %q", name)
	}
	return nil
}

type tartEntry struct {
	Source   string  `json:"Source"`
	Name     string  `json:"Name"`
	Size     float64 `json:"Size"` // GB
	Disk     float64 `json:"Disk"`
	Running  bool    `json:"Running"`
	State    string  `json:"State"`
	Accessed string  `json:"Accessed"`
}

// listImages runs `tart list --format json`.
func listImages(ctx context.Context, c *Config) ([]wire.Image, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, resolveBin(c.TartBin), "list", "--format", "json")
	cmd.Env = childEnv()
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tart list: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	var entries []tartEntry
	if err := json.Unmarshal(out.Bytes(), &entries); err != nil {
		return nil, fmt.Errorf("parse tart list: %w", err)
	}
	imgs := make([]wire.Image, 0, len(entries))
	for _, e := range entries {
		running := e.Running || strings.EqualFold(e.State, "running")
		size := e.Size
		if size == 0 {
			size = e.Disk
		}
		imgs = append(imgs, wire.Image{Name: e.Name, Source: e.Source, SizeGB: size, Running: running, Accessed: e.Accessed})
	}
	sort.Slice(imgs, func(i, j int) bool { return imgs[i].Name < imgs[j].Name })
	return imgs, nil
}

// runStreaming runs a command and sends each output line to progress.
func runStreaming(ctx context.Context, progress func(string), name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = childEnv()
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		// tart prints progress with carriage returns; split on both.
		sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
			if atEOF && len(data) == 0 {
				return 0, nil, nil
			}
			if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
				return i + 1, data[:i], nil
			}
			if atEOF {
				return len(data), data, nil
			}
			return 0, nil, nil
		})
		var last time.Time
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || time.Since(last) < time.Second {
				continue
			}
			last = time.Now()
			progress(line)
		}
	}()
	err := cmd.Wait()
	pw.Close()
	<-done
	return err
}

func tartPull(ctx context.Context, c *Config, image string, progress func(string)) error {
	if err := validImage(image); err != nil {
		return err
	}
	return runStreaming(ctx, progress, resolveBin(c.TartBin), "pull", image)
}

func tartDelete(ctx context.Context, c *Config, image string) error {
	if err := validImage(image); err != nil {
		return err
	}
	imgs, err := listImages(ctx, c)
	if err != nil {
		return err
	}
	for _, im := range imgs {
		if im.Name == image && im.Running {
			return errors.New("image is in use by a running VM")
		}
	}
	var errb bytes.Buffer
	cmd := exec.CommandContext(ctx, resolveBin(c.TartBin), "delete", image)
	cmd.Env = childEnv()
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tart delete: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// tartPrune deletes images that are not running and were last accessed more
// than OlderThanDays ago. With DryRun it only reports what would go.
func tartPrune(ctx context.Context, c *Config, p wire.TartPruneParams, progress func(string)) (string, error) {
	if p.OlderThanDays < 1 {
		return "", errors.New("older_than_days must be at least 1")
	}
	imgs, err := listImages(ctx, c)
	if err != nil {
		return "", err
	}
	cutoff := time.Now().AddDate(0, 0, -p.OlderThanDays)
	var removed []string
	for _, im := range imgs {
		if im.Running || im.Accessed == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, im.Accessed)
		if err != nil || t.After(cutoff) {
			continue
		}
		if p.DryRun {
			progress("would delete " + im.Name)
			removed = append(removed, im.Name)
			continue
		}
		if err := tartDelete(ctx, c, im.Name); err != nil {
			progress("skip " + im.Name + ": " + err.Error())
			continue
		}
		progress("deleted " + im.Name)
		removed = append(removed, im.Name)
	}
	verb := "deleted"
	if p.DryRun {
		verb = "would delete"
	}
	return fmt.Sprintf("%s %d images", verb, len(removed)), nil
}
