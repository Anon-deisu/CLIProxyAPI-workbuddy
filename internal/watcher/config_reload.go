// config_reload.go implements debounced configuration hot reload.
// It detects material changes and reloads clients when the config changes.
package watcher

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/diff"
	"gopkg.in/yaml.v3"

	log "github.com/sirupsen/logrus"
)

var loadConfigFileFunc = loadConfigFileWithSourceHash

const maxConfigLoadAttempts = 3

func loadConfigFileWithSourceHash(path string) (*config.Config, error) {
	for attempt := 0; attempt < maxConfigLoadAttempts; attempt++ {
		before, errRead := os.ReadFile(path)
		if errRead != nil {
			return nil, errRead
		}

		loaded, errLoad := config.LoadConfig(path)
		if errLoad != nil {
			return nil, errLoad
		}

		after, errRead := os.ReadFile(path)
		if errRead != nil {
			return nil, errRead
		}
		if !bytes.Equal(before, after) {
			continue
		}
		if loaded.SourceHash == "" {
			loaded.SourceHash = config.ComputeSourceHash(before)
		}
		return loaded, nil
	}
	return nil, fmt.Errorf("config file %q changed during each of %d load attempts", path, maxConfigLoadAttempts)
}

func (w *Watcher) stopConfigReloadTimer() {
	w.configReloadMu.Lock()
	if w.configReloadTimer != nil {
		w.configReloadTimer.Stop()
		w.configReloadTimer = nil
	}
	w.configReloadMu.Unlock()
}

func (w *Watcher) scheduleConfigReload() {
	w.configReloadMu.Lock()
	defer w.configReloadMu.Unlock()
	if w.configReloadTimer != nil {
		w.configReloadTimer.Stop()
	}
	w.configReloadTimer = time.AfterFunc(configReloadDebounce, func() {
		w.configReloadMu.Lock()
		w.configReloadTimer = nil
		w.configReloadMu.Unlock()
		w.reloadConfigIfChanged()
	})
}

// ReloadConfigIfChanged runs the same config reload path used by filesystem events.
func (w *Watcher) ReloadConfigIfChanged() {
	if w == nil {
		return
	}
	w.reloadConfigIfChanged()
}

func (w *Watcher) reloadConfigIfChanged() {
	// File events and post-save reloads may arrive together; an older read must
	// not apply after a newer one.
	w.configApplyMu.Lock()
	defer w.configApplyMu.Unlock()

	data, err := os.ReadFile(w.configPath)
	if err != nil {
		log.Errorf("failed to read config file for hash check: %v", err)
		return
	}
	if len(data) == 0 {
		log.Debugf("ignoring empty config file write event")
		return
	}
	sum := sha256.Sum256(data)
	newHash := hex.EncodeToString(sum[:])

	w.clientsMutex.RLock()
	currentHash := w.lastConfigHash
	w.clientsMutex.RUnlock()

	if currentHash != "" && currentHash == newHash {
		log.Debugf("config file content unchanged (hash match), skipping reload")
		return
	}
	log.Infof("config file changed, reloading: %s", w.configPath)
	if w.reloadConfig() {
		// Record the bytes the reload actually parsed, not the change-check read or a
		// later re-read: a write landing between them must not count as applied. A
		// write-back during load only costs one extra reload.
		w.clientsMutex.Lock()
		appliedHash := newHash
		if w.config != nil && w.config.SourceHash != "" {
			appliedHash = w.config.SourceHash
		}
		w.lastConfigHash = appliedHash
		w.clientsMutex.Unlock()
		w.persistConfigAsync()
	}
}

func (w *Watcher) reloadConfig() bool {
	log.Debug("=========================== CONFIG RELOAD ============================")
	log.Debugf("starting config reload from: %s", w.configPath)

	newConfig, errLoadConfig := loadConfigFileFunc(w.configPath)
	if errLoadConfig != nil {
		log.Errorf("failed to reload config: %v", errLoadConfig)
		return false
	}
	if newConfig.SourceHash == "" {
		// Fill a missing source hash before client callbacks can save a newer config.
		if data, errRead := os.ReadFile(w.configPath); errRead == nil && len(data) > 0 {
			newConfig.SourceHash = config.ComputeSourceHash(data)
		} else if errRead != nil {
			log.WithError(errRead).Debug("failed to compute loaded config hash")
		}
	}

	if w.mirroredAuthDir != "" {
		newConfig.AuthDir = w.mirroredAuthDir
	} else {
		if resolvedAuthDir, errResolveAuthDir := util.ResolveAuthDir(newConfig.AuthDir); errResolveAuthDir != nil {
			log.Errorf("failed to resolve auth directory from config: %v", errResolveAuthDir)
		} else {
			newConfig.AuthDir = resolvedAuthDir
		}
	}

	w.clientsMutex.Lock()
	var oldConfig *config.Config
	_ = yaml.Unmarshal(w.oldConfigYaml, &oldConfig)
	w.oldConfigYaml, _ = yaml.Marshal(newConfig)
	w.config = newConfig
	w.clientsMutex.Unlock()

	var affectedOAuthProviders []string
	if oldConfig != nil {
		_, affectedOAuthProviders = diff.DiffOAuthExcludedModelChanges(oldConfig.OAuthExcludedModels, newConfig.OAuthExcludedModels)
	}

	util.SetLogLevel(newConfig)
	if oldConfig != nil && oldConfig.Debug != newConfig.Debug {
		log.Debugf("log level updated - debug mode changed from %t to %t", oldConfig.Debug, newConfig.Debug)
	}

	if oldConfig != nil {
		details := diff.BuildConfigChangeDetails(oldConfig, newConfig)
		if len(details) > 0 {
			log.Info("config changes detected:")
			for _, d := range details {
				log.Infof("  %s", d)
			}
		} else {
			log.Debugf("no material config field changes detected")
		}
	}

	authDirChanged := oldConfig == nil || oldConfig.AuthDir != newConfig.AuthDir
	retryConfigChanged := oldConfig != nil && (oldConfig.RequestRetry != newConfig.RequestRetry || oldConfig.MaxRetryInterval != newConfig.MaxRetryInterval || oldConfig.MaxRetryCredentials != newConfig.MaxRetryCredentials)
	forceAuthRefresh := oldConfig != nil && (oldConfig.ForceModelPrefix != newConfig.ForceModelPrefix ||
		!reflect.DeepEqual(oldConfig.OAuthModelAlias, newConfig.OAuthModelAlias) ||
		!reflect.DeepEqual(oldConfig.OAuthSettings, newConfig.OAuthSettings) ||
		!reflect.DeepEqual(oldConfig.MetaKey, newConfig.MetaKey) ||
		!reflect.DeepEqual(oldConfig.Devin, newConfig.Devin) || retryConfigChanged)

	log.Infof("config successfully reloaded, triggering client reload")
	w.reloadClients(authDirChanged, affectedOAuthProviders, forceAuthRefresh)
	return true
}
