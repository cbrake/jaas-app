package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"syscall"
)

const odooOwnerMapMaxBytes = 16 << 10

// Only the digest is persisted. The private correspondence is not exposed by
// configuration diagnostics and never changes the authenticated Meet identity.
type odooOwnerMap struct {
	emails map[string]string
	digest string
}

type odooOwnerMapEntry struct {
	MeetEmail  string `json:"meet_email"`
	OdooUserID int64  `json:"odoo_user_id"`
	OdooEmail  string `json:"odoo_email"`
}

type odooOwnerMapDocument struct {
	Version      int                 `json:"version"`
	OdooBaseURL  string              `json:"odoo_base_url"`
	OdooDatabase string              `json:"odoo_database"`
	OdooSourceID string              `json:"odoo_source_id"`
	Owners       []odooOwnerMapEntry `json:"owners"`
}

var errOdooOwnerMap = errors.New("ODOO_OWNER_MAP_FILE is invalid")

// Open without following the final symlink or blocking on a FIFO. Check the
// opened inode, not a preceding stat, and bound both its size and the read.
// Group-read supports the existing root:service-group 0440 secret mounts.
func loadOdooOwnerMap(path string, cfg *Config) (*odooOwnerMap, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errOdooOwnerMap
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0137 != 0 || info.Size() < 1 || info.Size() > odooOwnerMapMaxBytes {
		return nil, errOdooOwnerMap
	}
	data, err := io.ReadAll(io.LimitReader(f, odooOwnerMapMaxBytes+1))
	if err != nil || len(data) > odooOwnerMapMaxBytes {
		return nil, errOdooOwnerMap
	}
	return parseOdooOwnerMap(data, cfg)
}

// encoding/json permits duplicate and case-insensitive struct keys. Require
// exact, unique keys at both object levels before decoding the typed document.
func odooOwnerMapObject(data []byte, keys ...string) (map[string]json.RawMessage, error) {
	allowed := make(map[string]bool, len(keys))
	for _, key := range keys {
		allowed[key] = true
	}
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errOdooOwnerMap
	}
	fields := make(map[string]json.RawMessage, len(keys))
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		if err != nil || !ok || !allowed[key] || fields[key] != nil {
			return nil, errOdooOwnerMap
		}
		var raw json.RawMessage
		if d.Decode(&raw) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, errOdooOwnerMap
		}
		fields[key] = raw
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return nil, errOdooOwnerMap
	}
	var extra any
	if d.Decode(&extra) != io.EOF || len(fields) != len(keys) {
		return nil, errOdooOwnerMap
	}
	return fields, nil
}

func parseOdooOwnerMap(data []byte, cfg *Config) (*odooOwnerMap, error) {
	if len(data) > odooOwnerMapMaxBytes {
		return nil, errOdooOwnerMap
	}
	fields, err := odooOwnerMapObject(data, "version", "odoo_base_url", "odoo_database", "odoo_source_id", "owners")
	if err != nil {
		return nil, errOdooOwnerMap
	}
	var entries []json.RawMessage
	if json.Unmarshal(fields["owners"], &entries) != nil || len(entries) == 0 || len(entries) > 128 {
		return nil, errOdooOwnerMap
	}
	for _, entry := range entries {
		if _, err := odooOwnerMapObject(entry, "meet_email", "odoo_user_id", "odoo_email"); err != nil {
			return nil, errOdooOwnerMap
		}
	}
	var doc odooOwnerMapDocument
	if json.Unmarshal(data, &doc) != nil || doc.Version != 1 {
		return nil, errOdooOwnerMap
	}
	doc.OdooBaseURL = strings.TrimRight(strings.TrimSpace(doc.OdooBaseURL), "/")
	doc.OdooDatabase = strings.TrimSpace(doc.OdooDatabase)
	doc.OdooSourceID = strings.TrimSpace(doc.OdooSourceID)
	if doc.OdooBaseURL != cfg.OdooBaseURL || doc.OdooDatabase != cfg.OdooDatabase || doc.OdooSourceID != cfg.OdooSourceID {
		return nil, errOdooOwnerMap
	}
	m := &odooOwnerMap{emails: make(map[string]string, len(doc.Owners))}
	odooEmails := make(map[string]bool, len(doc.Owners))
	userIDs := make(map[int64]bool, len(doc.Owners))
	for i := range doc.Owners {
		owner := &doc.Owners[i]
		owner.MeetEmail = normalizeEmail(owner.MeetEmail)
		owner.OdooEmail = normalizeEmail(owner.OdooEmail)
		if owner.MeetEmail == "" || owner.OdooEmail == "" || owner.OdooUserID < 1 || m.emails[owner.MeetEmail] != "" || odooEmails[owner.OdooEmail] || userIDs[owner.OdooUserID] {
			return nil, errOdooOwnerMap
		}
		m.emails[owner.MeetEmail] = owner.OdooEmail
		odooEmails[owner.OdooEmail], userIDs[owner.OdooUserID] = true, true
	}
	// Formatting and row order do not change the binding, but either endpoint,
	// destination identity or expected Odoo account ID does.
	sort.Slice(doc.Owners, func(i, j int) bool { return doc.Owners[i].MeetEmail < doc.Owners[j].MeetEmail })
	canonical, err := json.Marshal(doc)
	if err != nil {
		return nil, errOdooOwnerMap
	}
	digest := sha256.Sum256(canonical)
	m.digest = "map-v1:" + hex.EncodeToString(digest[:])
	return m, nil
}

func (cfg *Config) odooOwnerMapDigest() string {
	if cfg.odooOwnerMap == nil {
		return "direct-v1"
	}
	return cfg.odooOwnerMap.digest
}

func (cfg *Config) mapOdooSnapshotOwner(p *odooSnapshot) error {
	if p.Operation == "delete" || cfg.odooOwnerMap == nil {
		return nil
	}
	email, ok := cfg.odooOwnerMap.emails[p.OwnerEmail]
	if !ok {
		return errors.New("owner_mapping_missing")
	}
	p.OwnerEmail = email
	return nil
}
