package catalog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Lock coordinates the control panel and CLI catalogue writers. Reads use atomic snapshots.
func Lock(path string) (func(), error) {
	if path == "" {
		return nil, errors.New("catalogue file is not configured")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	lock := path + ".lock"
	if err := os.Mkdir(lock, 0700); err != nil {
		return nil, fmt.Errorf("catalogue is being modified, or a stale lock needs removal (%s): %w", lock, err)
	}
	return func() { os.Remove(lock) }, nil
}
func (l *Live) Import(o AddOptions) (Item, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.path == "" || l.filesDir == "" {
		return Item{}, errors.New("catalogue upload is not configured")
	}
	unlock, err := Lock(l.path)
	if err != nil {
		return Item{}, err
	}
	defer unlock()
	c, err := Load(l.path, l.filesDir)
	if err != nil {
		return Item{}, err
	}
	it, err := c.Add(o)
	if err != nil {
		return Item{}, err
	}
	if err = c.Save(); err != nil {
		return Item{}, err
	}
	l.cur = c
	l.checked = time.Now()
	if st, err := os.Stat(l.path); err == nil {
		l.modTime = st.ModTime()
	}
	return it, nil
}
