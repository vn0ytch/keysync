// Copyright 2016 Square Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package keysync

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/square/go-sq-metrics"
)

// secretState records the state of a secret we've written
type secretState struct {
	// ContentHash is a Sha256 of what we wrote out, used to detect content corruption in the filesystem
	ContentHash [sha256.Size]byte
	// Checksum is the server's identifier for the contents of the hash (it's an HMAC)
	Checksum string
	// We store the mode we wrote
	FileInfo
}

type syncerEntry struct {
	Client
	ClientConfig
	WriteConfig
	SyncState map[string]secretState
}

// A Syncer manages a collection of clients, handling downloads and writing out updated secrets.
// Construct one using the NewSyncer and AddClient functions
type Syncer struct {
	config        *Config
	server        *url.URL
	clients       map[string]syncerEntry
	logger        *logrus.Entry
	metricsHandle *sqmetrics.SquareMetrics
	syncMutex     sync.Mutex
}

// NewSyncer instantiates the main stateful object in Keysync.
func NewSyncer(config *Config, logger *logrus.Entry, metricsHandle *sqmetrics.SquareMetrics) (*Syncer, error) {
	syncer := Syncer{config: config, clients: map[string]syncerEntry{}, logger: logger, metricsHandle: metricsHandle}
	serverUrl, err := url.Parse("https://" + config.Server)
	if err != nil {
		return nil, fmt.Errorf("Failed parsing server: %s", config.Server)
	}
	syncer.server = serverUrl
	return &syncer, nil
}

// LoadClients gets configured clients,
func (s *Syncer) LoadClients() error {
	newConfigs, err := s.config.LoadClients()
	if err != nil {
		return err
	}
	s.logger.WithField("count", len(newConfigs)).Info("Loaded configs")

	for name, clientConfig := range newConfigs {
		// If there's already a client loaded, reload it
		syncerEntry, ok := s.clients[name]
		if ok {
			if syncerEntry.ClientConfig == clientConfig {
				// Exists, and the same config.
				syncerEntry.Client.RebuildClient()
				continue
			}
		}
		// Otherwise we (re)create the client
		client, err := s.buildClient(name, clientConfig, s.metricsHandle)
		if err != nil {
			s.logger.WithError(err).WithField("client", name).Error("Failed building client")
			continue

		}
		s.clients[name] = *client
	}
	for name, client := range s.clients {
		// TODO: Record for cleanup. We don't want to actually do it in this function, so we record it for the
		// next sync call to take care of it.
		_, ok := newConfigs[name]
		if !ok {
			s.logger.Warnf("Client gone: %s (%v)", name, client)
		}
	}
	return nil
}

// buildClient collects the configuration and builds a client.  Most of this code should probably be refactored ito NewClient
func (s *Syncer) buildClient(name string, clientConfig ClientConfig, metricsHandle *sqmetrics.SquareMetrics) (*syncerEntry, error) {
	clientLogger := s.logger.WithField("client", name)
	client, err := NewClient(clientConfig.Cert, clientConfig.Key, s.config.CaFile, s.server, time.Minute, clientLogger, metricsHandle)
	if err != nil {
		return nil, err
	}
	user := clientConfig.User
	group := clientConfig.Group
	if user == "" {
		user = s.config.DefaultUser
	}
	if group == "" {
		group = s.config.DefaultGroup
	}
	defaultOwnership, err := NewOwnership(user, group)
	if err != nil {
		// We log an error here but continue on.  The default of "0", root, is safe.
		s.logger.WithError(err).Error("Failed getting default ownership")
	}
	writeConfig := WriteConfig{
		WriteDirectory:    filepath.Join(s.config.SecretsDir, clientConfig.DirName),
		EnforceFilesystem: s.config.FsType,
		ChownFiles:        s.config.ChownFiles,
		DefaultOwnership:  defaultOwnership,
	}
	return &syncerEntry{client, clientConfig, writeConfig, map[string]secretState{}}, nil
}

// Randomize the sleep interval, increasing up to 1/4 of the duration.
func randomize(d time.Duration) time.Duration {
	maxAdded := float64(d) / 4
	amount := rand.Float64() * maxAdded

	return time.Duration(float64(d) + amount)
}

// Run the main sync loop.
func (s *Syncer) Run() error {
	pollInterval, err := time.ParseDuration(s.config.PollInterval)
	if s.config.PollInterval != "" && err != nil {
		return fmt.Errorf("Couldn't parse Poll Interval '%s': %v\n", s.config.PollInterval, err)
	}

	for {
		err = s.RunOnce()
		if err != nil {
			s.logger.WithError(err).Error("Failed running sync")
		}

		// No poll interval configured, so return now
		if s.config.PollInterval == "" {
			return err
		}
		sleep := randomize(pollInterval)
		s.logger.WithField("duration", sleep).Info("Sleeping")
		time.Sleep(sleep)
	}
}

// RunOnce runs the syncer once, for all clients, without sleeps.
func (s *Syncer) RunOnce() error {
	s.syncMutex.Lock()
	defer s.syncMutex.Unlock()
	err := s.LoadClients()
	if err != nil {
		return err
	}
	for name, entry := range s.clients {
		err = entry.Sync()
		if err != nil {
			// Record error but continue updating other clients
			s.logger.WithError(err).WithField("name", name).Error("Failed while syncing")
		}
	}
	return nil
}

// Sync this: Download and write all secrets.
func (entry *syncerEntry) Sync() error {
	err := os.MkdirAll(entry.WriteDirectory, 0775)
	if err != nil {
		return fmt.Errorf("Making client directory '%s': %v", entry.WriteDirectory, err)
	}
	secrets, ok := entry.Client.SecretList()
	if !ok {
		// SecretList logged the error.  We return as there's nothing more we can do.
		return nil
	}

	pendingDeletions := []string{}
	for name, secretMetadata := range secrets {
		if entry.IsValidOnDisk(secretMetadata) {
			// The secret is already downloaded, so no action needed
			entry.logger.WithField("secret", name).Warn("Not requesting still-valid secret")
			continue
		}
		secret, err := entry.Client.Secret(name)
		if err != nil {
			// This is essentially a race condition: A secret was deleted between listing and fetching
			if _, deleted := err.(SecretDeleted); deleted {
				// We defer actual deletion to the loop below, so that new secrets are always written
				// before any are deleted.
				pendingDeletions = append(pendingDeletions, name)
			} else {
				// There was some other error talking to the server.
				// We put a value in syncState so we don't delete it as an unknown file.
				entry.SyncState[name] = secretState{}
			}
			continue
		}
		fileinfo, err := atomicWrite(secret.Name, secret, entry.WriteConfig)
		if err != nil {
			entry.logger.WithError(err).WithField("file", secret.Name).Error("Failed while writing secret")
			// This situation is unlikely: We couldn't write the secret to disk.
			// If atomicWrite fails, then no changes to the secret on-disk were made, thus we make no change
			// to the entry.SyncState
			continue
		}

		// Success!  Store the state we wrote to disk for later validation.
		entry.logger.WithField("file", secret.Name).WithField("dir", entry.WriteDirectory).Info("Wrote file")
		entry.SyncState[secret.Name] = secretState{sha256.Sum256(secret.Content), secret.Checksum, *fileinfo}
	}
	// For all secrets we've previously synced, remove state for ones not returned
	for name, _ := range entry.SyncState {
		if _, present := secrets[name]; !present {
			pendingDeletions = append(pendingDeletions, name)
		}
	}
	for _, name := range pendingDeletions {
		entry.logger.WithField("secret", name).Info("Removing old secret")
		delete(entry.SyncState, name)
		os.Remove(filepath.Join(entry.WriteDirectory, name))
	}

	fileInfos, err := ioutil.ReadDir(entry.WriteDirectory)
	if err != nil {
		return fmt.Errorf("Couldn't read directory: %s\n", entry.WriteDirectory)
	}
	for _, fileInfo := range fileInfos {
		existingFile := fileInfo.Name()
		if _, present := entry.SyncState[existingFile]; !present {
			// This file wasn't written in the loop above, so we remove it.
			entry.logger.WithField("file", existingFile).Info("Removing unknown file")
			os.Remove(filepath.Join(entry.WriteDirectory, existingFile))
		}
	}
	return nil
}

// IsValidOnDisk verifies the secret is written to disk with the correct content, permissions, and ownership
func (s *syncerEntry) IsValidOnDisk(secret Secret) bool {
	state := s.SyncState[secret.Name]
	if state.Checksum != secret.Checksum {
		return false
	}
	path := filepath.Join(s.WriteDirectory, secret.Name)
	// Check on-disk permissions, and ownership against what's configured.
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	fileinfo, err := GetFileInfo(f)
	if err != nil {
		return false
	}
	if state.FileInfo != *fileinfo {
		return false
	}

	// Check the content of what's on disk
	var b bytes.Buffer
	_, err = b.ReadFrom(f)
	if err != nil {
		return false
	}
	hash := sha256.Sum256(b.Bytes())

	if state.ContentHash != hash {
		// As tempting as it is, we shouldn't log hashes as they'd leak information about the secret.
		s.logger.WithField("secret", secret.Name).Warnf("Secret modified on disk?")
		return false
	}

	// OK, the file is unchanged
	return true
}
