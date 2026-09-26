package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const cacheTTL = 24 * time.Hour
const cacheMaxBytes int64 = 256 << 20
const cacheMaxEntries = 10000

type cacheRecord struct {
	Key, Partition, Original, Forwarded, Owner, Handle string
	Expires                                            time.Time
}

type decisionCache struct {
	mu      sync.Mutex
	dir     string
	aead    cipher.AEAD
	macKey  []byte
	records map[string]cacheRecord
	sizes   map[string]int64
}

func deriveKey(secret []byte, label string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("bifrost-headroom/" + label + "/v1"))
	return m.Sum(nil)
}

func openDecisionCache(dir string, secret []byte) (*decisionCache, error) {
	if dir == "" {
		return nil, nil
	}
	if !filepath.IsAbs(dir) {
		return nil, errors.New("cache_dir must be an absolute path")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, errors.New("cannot initialize headroom cache")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, errors.New("cannot secure headroom cache")
	}
	block, err := aes.NewCipher(deriveKey(secret, "cache-encryption"))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	c := &decisionCache{dir: dir, aead: aead, macKey: deriveKey(secret, "cache-handles"), records: map[string]cacheRecord{}, sizes: map[string]int64{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, errors.New("cannot read headroom cache")
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".cache") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, errors.New("cannot read headroom cache record")
		}
		r, readErr := c.decode(data)
		if readErr != nil {
			return nil, errors.New("cannot decrypt headroom cache record")
		}
		info, readErr := e.Info()
		if readErr != nil {
			return nil, errors.New("cannot inspect headroom cache record")
		}
		c.records[r.Key], c.sizes[r.Key] = r, info.Size()
	}
	if err := c.pruneExpiredLocked(time.Now()); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *decisionCache) id(partition, original string) string {
	m := hmac.New(sha256.New, c.macKey)
	m.Write([]byte("decision\x00" + partition + "\x00" + original))
	return hex.EncodeToString(m.Sum(nil))
}
func (c *decisionCache) handle(owner, original string) string {
	m := hmac.New(sha256.New, c.macKey)
	m.Write([]byte("retrieve\x00" + owner + "\x00" + original))
	return hex.EncodeToString(m.Sum(nil))
}
func (c *decisionCache) encode(r cacheRecord) ([]byte, error) {
	plain, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return append(nonce, c.aead.Seal(nil, nonce, plain, []byte("bifrost-headroom-cache-v1"))...), nil
}
func (c *decisionCache) decode(data []byte) (cacheRecord, error) {
	var r cacheRecord
	n := c.aead.NonceSize()
	if len(data) < n {
		return r, errors.New("short cache record")
	}
	plain, err := c.aead.Open(nil, data[:n], data[n:], []byte("bifrost-headroom-cache-v1"))
	if err != nil {
		return r, err
	}
	err = json.Unmarshal(plain, &r)
	if err != nil || r.Key == "" {
		return r, errors.New("invalid cache record")
	}
	return r, nil
}
func (c *decisionCache) pruneExpiredLocked(now time.Time) error {
	for key, r := range c.records {
		if !now.Before(r.Expires) {
			if err := os.Remove(filepath.Join(c.dir, key+".cache")); err != nil && !os.IsNotExist(err) {
				return errors.New("cannot expire headroom cache record")
			}
			delete(c.records, key)
			delete(c.sizes, key)
		}
	}
	return nil
}
func (c *decisionCache) lookup(partition, original string) (cacheRecord, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.pruneExpiredLocked(time.Now()); err != nil {
		return cacheRecord{}, false, err
	}
	r, ok := c.records[c.id(partition, original)]
	return r, ok, nil
}
func (c *decisionCache) bestPrefix(partition, original string) (cacheRecord, bool) {
	var best cacheRecord
	for _, r := range c.records {
		if r.Partition == partition && len(r.Original) > len(best.Original) && strings.HasPrefix(original, r.Original) {
			best = r
		}
	}
	return best, best.Key != ""
}
func (c *decisionCache) put(partition, owner, original, forwarded string, ccr bool) (cacheRecord, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.pruneExpiredLocked(time.Now()); err != nil {
		return cacheRecord{}, err
	}
	key := c.id(partition, original)
	if r, ok := c.records[key]; ok {
		return r, nil
	}
	r := cacheRecord{Key: key, Partition: partition, Original: original, Forwarded: forwarded, Owner: owner, Expires: time.Now().Add(cacheTTL)}
	if ccr {
		r.Handle = c.handle(owner, original)
	}
	data, err := c.encode(r)
	if err != nil {
		return cacheRecord{}, err
	}
	var total int64
	for _, n := range c.sizes {
		total += n
	}
	if len(c.records) >= cacheMaxEntries || total+int64(len(data)) > cacheMaxBytes {
		return cacheRecord{}, errors.New("headroom cache capacity reached")
	}
	tmp, err := os.CreateTemp(c.dir, ".headroom-cache-")
	if err != nil {
		return cacheRecord{}, errors.New("cannot write headroom cache")
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, filepath.Join(c.dir, key+".cache"))
	}
	if err == nil {
		var d *os.File
		d, err = os.Open(c.dir)
		if err == nil {
			err = d.Sync()
			d.Close()
		}
	}
	if err != nil {
		return cacheRecord{}, errors.New("cannot persist headroom cache")
	}
	c.records[key], c.sizes[key] = r, int64(len(data))
	return r, nil
}
func (c *decisionCache) retrieve(handle, owner string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pruneExpiredLocked(time.Now()) != nil {
		return "", false
	}
	keys := make([]string, 0, len(c.records))
	for k := range c.records {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r := c.records[k]
		if hmac.Equal([]byte(r.Handle), []byte(handle)) && hmac.Equal([]byte(r.Owner), []byte(owner)) {
			return r.Original, true
		}
	}
	return "", false
}
