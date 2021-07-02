package binary_plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/konveyor/crane-lib/transform"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type BinaryPlugin struct {
	commandRunner
	name string
	log  logrus.FieldLogger
}

func NewBinaryPlugin(path string) transform.Plugin {
	return &BinaryPlugin{
		commandRunner: &binaryRunner{pluginPath: path},
		name:          filepath.Base(path),
		log:           logrus.New().WithField("pluginPath", path),
	}
}

func (b *BinaryPlugin) Run(u *unstructured.Unstructured, extras map[string]string) (transform.PluginResponse, error) {
	p := transform.PluginResponse{}
	logs := []string{}

	out, errBytes, err := b.commandRunner.Run(u, b.log)
	if err != nil {
		b.log.Errorf("error running the plugin command")
		return p, fmt.Errorf("error running the plugin command: %v", err)
	}

	if len(errBytes) != 0 {
		logs = strings.Split(string(errBytes), "\n")
		for _, line := range logs {
			b.log.Debug("Plugin Log line: ", line)
		}
	}

	err = json.Unmarshal(out, &p)
	if err != nil {
		b.log.Errorf("unable to decode json sent by the plugin")
		return p, fmt.Errorf("unable to decode json sent by the plugin: %s, err: %v", string(out), err)
	}

	return p, nil
}

func (b *BinaryPlugin) Metadata() (transform.PluginMetadata, error) {
	_, _, err := b.commandRunner.Metadata(b.log)
	if err != nil {
		return transform.PluginMetadata{}, err
	}

	return transform.PluginMetadata{}, nil
}

type commandRunner interface {
	Run(u *unstructured.Unstructured, log logrus.FieldLogger) ([]byte, []byte, error)
	Metadata(log logrus.FieldLogger) ([]byte, []byte, error)
}

type binaryRunner struct {
	pluginPath string
}

func (b *binaryRunner) Metadata(log logrus.FieldLogger) ([]byte, []byte, error) {
	return nil, nil, nil
}

func (b *binaryRunner) Run(u *unstructured.Unstructured, log logrus.FieldLogger) ([]byte, []byte, error) {
	objJson, err := u.MarshalJSON()
	if err != nil {
		log.Errorf("unable to marshal unstructured Object")
		return nil, nil, fmt.Errorf("unable to marshal unstructured Object: %s, err: %v", u, err)
	}

	command := exec.Command(b.pluginPath)

	// set var to get the output
	var out bytes.Buffer
	var errorBytes bytes.Buffer

	// set the output to our variable
	command.Stdout = &out
	command.Stdin = bytes.NewBuffer(objJson)
	command.Stderr = &errorBytes
	err = command.Run()
	if err != nil {
		log.Errorf("unable to run the plugin binary")
		return nil, nil, fmt.Errorf("unable to run the plugin binary, err: %v", err)
	}

	return out.Bytes(), errorBytes.Bytes(), nil
}
