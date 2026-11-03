package bgpbypass

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
)

type savedState struct {
	Version      int           `json:"version"`
	Associations []association `json:"associations"`
}

func (s *Service) loadState() {
	path := s.config.options.StatePath
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		s.logger.Warn("read state: ", err)
		return
	}
	defer f.Close()
	// Bound corrupt or hostile snapshots independently of the JSON decoder.
	limit := int64(s.config.options.Limits.MaxDNSAssociations)*1024 + 1024
	info, err := f.Stat()
	if err != nil || info.Size() > limit {
		s.logger.Warn("state exceeds size limit")
		return
	}
	var saved savedState
	dec := json.NewDecoder(io.LimitReader(f, limit))
	dec.DisallowUnknownFields()
	err = dec.Decode(&saved)
	if err == nil {
		var trailing any
		if dec.Decode(&trailing) != io.EOF {
			err = E.New("trailing state content")
		}
	}
	if err != nil || saved.Version != 1 || len(saved.Associations) > s.config.options.Limits.MaxDNSAssociations {
		s.logger.Warn("invalid state; starting cold")
		return
	}
	now := time.Now()
	for _, a := range saved.Associations {
		if len(a.Domain) > 253 || a.Domain == "" || a.Expires.After(now.Add(time.Duration(^uint32(0))*time.Second)) {
			continue
		}
		a.Domain = domainName(a.Domain)
		a.Address = a.Address.Unmap()
		s.state.learn(a, now)
	}
}

func (s *Service) saveState() {
	path := s.config.options.StatePath
	if path == "" {
		return
	}
	if err := writeState(path, savedState{Version: 1, Associations: s.state.snapshot(time.Now())}); err != nil {
		s.logger.Warn("save state: ", err)
	}
}

func writeState(path string, state savedState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".bgp-bypass-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = json.NewEncoder(f).Encode(state); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return commitStateFile(name, path)
}
