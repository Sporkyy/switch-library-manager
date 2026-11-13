package settings

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/magiconair/properties"
	"go.uber.org/zap"
)

var (
	keysInstance *switchKeys
)

type switchKeys struct {
	keys map[string]string
}

// String keeps key material out of logs and error messages that format a key
// set with %v or %s; only the number of loaded keys is shown.
func (k *switchKeys) String() string {
	if k == nil {
		return "switchKeys(nil)"
	}
	return fmt.Sprintf("switchKeys(%d keys, values redacted)", len(k.keys))
}

// GoString applies the same redaction to %#v.
func (k *switchKeys) GoString() string { return k.String() }

func (k *switchKeys) GetKey(keyName string) string {
	return k.keys[keyName]
}

func SwitchKeys() (*switchKeys, error) {
	return keysInstance, nil
}

func InitSwitchKeys(baseFolder string) (*switchKeys, error) {
	// A failed lookup must not leave keys from a previous base folder active.
	keysInstance = nil
	var (
		path string
		p    *properties.Properties
		err  error
	)
	logger := zap.S()

	// first, try to read the prod keys from the settings value
	settings, settingsErr := ReadSettings(baseFolder)
	if settingsErr != nil {
		return nil, settingsErr
	}
	if settings.Paths.ProdKeys != "" {
		path = settings.Paths.ProdKeys
		if !strings.EqualFold(filepath.Ext(path), ".keys") {
			path = filepath.Join(path, "prod.keys")
		}

		logger.Infof("Trying to load prod.keys based on settings.json: %v", path)
		p, err = properties.LoadFile(path, properties.UTF8)
	} else {
		err = errors.New("prod.keys not defined in settings.json")
	}

	// second, if not found by settings look into the current folder
	if err != nil {
		path = filepath.Join(baseFolder, "prod.keys")

		logger.Infof("Trying to load prod.keys based on current folder: %v", path)
		p, err = properties.LoadFile(path, properties.UTF8)
	}

	// third, if not found in the application folders, look in home directory
	if err != nil {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			err = homeErr
		} else {
			path = filepath.Join(home, ".switch", "prod.keys")

			logger.Infof("Trying to load prod.keys based on home directory: %v", path)
			p, err = properties.LoadFile(path, properties.UTF8)
		}
	}

	if err != nil {
		logger.Info("Unable to find prod.keys")
		return nil, errors.New("Error trying to read prod.keys [reason:" + err.Error() + "]")
	}

	keysInstance = &switchKeys{keys: map[string]string{}}
	for _, key := range p.Keys() {
		value, _ := p.Get(key)
		keysInstance.keys[key] = value
	}

	logger.Infof("Loaded prod.keys from: %v", path)
	return keysInstance, nil
}
