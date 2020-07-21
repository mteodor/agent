// Copyright (c) Mainflux
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"os"
	"strings"

	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"time"

	"github.com/mainflux/agent/pkg/agent"

	export "github.com/mainflux/export/pkg/config"
	errors "github.com/mainflux/mainflux/errors"
	log "github.com/mainflux/mainflux/logger"
	"github.com/mainflux/mainflux/things"
)

const exportConfigFile = "/configs/export/config.toml"

// Config represents the parameters for boostraping
type Config struct {
	URL           string
	ID            string
	Key           string
	Retries       string
	RetryDelaySec string
	Encrypt       string
	SkipTLS       bool
}

type ServicesConfig struct {
	Agent  agent.Config  `json:"agent"`
	Export export.Config `json:"export"`
}

type deviceConfig struct {
	MainfluxID       string           `json:"mainflux_id"`
	MainfluxKey      string           `json:"mainflux_key"`
	MainfluxChannels []things.Channel `json:"mainflux_channels"`
	ClientKey        string           `json:"client_key"`
	ClientCert       string           `json:"client_cert"`
	CaCert           string           `json:"ca_cert"`
	Content          ServicesConfig   `json:"content"`
}

type infraConfig struct {
	LogLevel     string        `json:"log_level"`
	HTTPPort     string        `json:"http_port"`
	MqttURL      string        `json:"mqtt_url"`
	EdgexURL     string        `json:"edgex_url"`
	NatsURL      string        `json:"nats_url"`
	ExportConfig export.Config `json:"export_config"`
}

// Bootstrap - Retrieve device config
func Bootstrap(cfg Config, logger log.Logger, file string) error {
	retries, err := strconv.ParseUint(cfg.Retries, 10, 64)
	if err != nil {
		return errors.New(fmt.Sprintf("Invalid BOOTSTRAP_RETRIES value: %s", err))
	}

	if retries == 0 {
		logger.Info("No bootstraping, environment variables will be used")
		return nil
	}

	retryDelaySec, err := strconv.ParseUint(cfg.RetryDelaySec, 10, 64)
	if err != nil {
		return errors.New(fmt.Sprintf("Invalid BOOTSTRAP_RETRY_DELAY_SECONDS value: %s", err))
	}

	logger.Info(fmt.Sprintf("Requesting config for %s from %s", cfg.ID, cfg.URL))

	dc := deviceConfig{}

	for i := 0; i < int(retries); i++ {
		dc, err = getConfig(cfg.ID, cfg.Key, cfg.URL, cfg.SkipTLS, logger)
		if err == nil {
			break
		}
		logger.Error(fmt.Sprintf("Fetching bootstrap failed with error: %s", err))
		logger.Debug(fmt.Sprintf("Retries remaining: %d. Retrying in %d seconds", retries, retryDelaySec))
		time.Sleep(time.Duration(retryDelaySec) * time.Second)
		if i == int(retries)-1 {
			logger.Warn("Retries exhausted")
			logger.Info(fmt.Sprintf("Continuing with local config"))
			return nil
		}
	}

	saveExportConfig(dc.Content.Export, logger)

	if len(dc.MainfluxChannels) < 2 {
		return agent.ErrMalformedEntity
	}

	ctrlChan := dc.MainfluxChannels[0].ID
	dataChan := dc.MainfluxChannels[1].ID
	if dc.MainfluxChannels[0].Metadata["type"] == "data" {
		ctrlChan = dc.MainfluxChannels[1].ID
		dataChan = dc.MainfluxChannels[0].ID
	}

	sc := dc.Content.Agent.Server
	cc := agent.ChanConf{
		Control: ctrlChan,
		Data:    dataChan,
	}
	ec := dc.Content.Agent.Edgex
	lc := dc.Content.Agent.Log

	mc := dc.Content.Agent.MQTT
	mc.Password = dc.MainfluxKey
	mc.Username = dc.MainfluxID
	mc.ClientCert = dc.ClientCert
	mc.ClientKey = dc.ClientKey
	mc.CaCert = dc.CaCert

	hc := dc.Content.Agent.Heartbeat
	tc := dc.Content.Agent.Terminal
	c := agent.NewConfig(sc, cc, ec, lc, mc, hc, tc, file)

	return agent.SaveConfig(c)
}

func saveExportConfig(econf export.Config, logger log.Logger) {
	if econf.File == "" {
		econf.File = exportConfigFile
	}
	exConfFileExist := false
	if _, err := os.Stat(econf.File); err == nil {
		exConfFileExist = true
		logger.Info(fmt.Sprintf("Export config file %s exists", econf.File))
	}
	if !exConfFileExist {
		logger.Info(fmt.Sprintf("Saving export config file %s", econf.File))
		if err := export.Save(econf); err != nil {
			logger.Warn(fmt.Sprintf("Failed to save export config file %s", err))
		}
	}
}

func getConfig(bsID, bsKey, bsSvrURL string, skipTLS bool, logger log.Logger) (deviceConfig, error) {
	// Get the SystemCertPool, continue with an empty pool on error
	rootCAs, err := x509.SystemCertPool()
	if err != nil {
		logger.Error(err.Error())
	}
	if rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}
	// Trust the augmented cert pool in our client
	config := &tls.Config{
		InsecureSkipVerify: skipTLS,
		RootCAs:            rootCAs,
	}
	tr := &http.Transport{TLSClientConfig: config}
	client := &http.Client{Transport: tr}
	url := fmt.Sprintf("%s/%s", bsSvrURL, bsID)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return deviceConfig{}, err
	}

	req.Header.Add("Authorization", bsKey)
	resp, err := client.Do(req)
	if err != nil {
		return deviceConfig{}, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return deviceConfig{}, errors.New(http.StatusText(resp.StatusCode))
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return deviceConfig{}, err
	}
	defer resp.Body.Close()
	dc := deviceConfig{}
	b := convertContentStringToJSON(body)
	fmt.Printf("%v\n", string(b))
	if err := json.Unmarshal([]byte(b), &dc); err != nil {
		return deviceConfig{}, err
	}
	return dc, nil
}

func convertContentStringToJSON(bin []byte) []byte {
	b := strings.ReplaceAll(string(bin), "\\", "")
	b = strings.ReplaceAll(string(b), "\"{", "{")
	b = strings.ReplaceAll(string(b), "}\"", "}")
	return []byte(b)
}
