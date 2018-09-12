package mbus

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/cloudfoundry/yagnats"

	"crypto/x509"
	"time"

	"crypto/tls"
	boshhandler "github.com/cloudfoundry/bosh-agent/handler"
	boshplatform "github.com/cloudfoundry/bosh-agent/platform"
	boshsettings "github.com/cloudfoundry/bosh-agent/settings"
	bosherr "github.com/cloudfoundry/bosh-utils/errors"
	boshlog "github.com/cloudfoundry/bosh-utils/logger"
	boshretry "github.com/cloudfoundry/bosh-utils/retrystrategy"
	"regexp"
)

const (
	responseMaxLength        = 1024 * 1024
	natsHandlerLogTag        = "NATS Handler"
	natsConnectionMaxRetries = 4
)

type Handler interface {
	Run(boshhandler.Func) error
	Start(boshhandler.Func) error
	RegisterAdditionalFunc(boshhandler.Func)
	Send(target boshhandler.Target, topic boshhandler.Topic, message interface{}) error
	Stop()
}

type natsHandler struct {
	settingsService boshsettings.Service
	client          yagnats.NATSClient
	platform        boshplatform.Platform

	handlerFuncs     []boshhandler.Func
	handlerFuncsLock sync.Mutex

	logger      boshlog.Logger
	auditLogger boshplatform.AuditLogger
	logTag      string
}

func NewNatsHandler(
	settingsService boshsettings.Service,
	client yagnats.NATSClient,
	logger boshlog.Logger,
	platform boshplatform.Platform,
) Handler {
	return &natsHandler{
		settingsService: settingsService,
		client:          client,
		platform:        platform,

		logger:      logger,
		logTag:      natsHandlerLogTag,
		auditLogger: platform.GetAuditLogger(),
	}
}

func (h *natsHandler) Run(handlerFunc boshhandler.Func) error {
	err := h.Start(handlerFunc)
	defer h.Stop()

	if err != nil {
		return bosherr.WrapError(err, "Starting nats handler")
	}

	h.runUntilInterrupted()

	return nil
}

func (h *natsHandler) Start(handlerFunc boshhandler.Func) error {
	h.RegisterAdditionalFunc(handlerFunc)

	connProvider, err := h.getConnectionInfo()
	if err != nil {
		return bosherr.WrapError(err, "Getting connection info")
	}

	h.client.BeforeConnectCallback(func() {
		hostSplit := strings.Split(connProvider.Addr, ":")
		ip := hostSplit[0]

		if net.ParseIP(ip) == nil {
			return
		}

		err = h.platform.DeleteARPEntryWithIP(ip)
		if err != nil {
			h.logger.Error(h.logTag, "Cleaning ip-mac address cache for: %s", ip)
		}
	})

	natsRetryable := boshretry.NewRetryable(func() (bool, error) {
		err := h.client.Connect(connProvider)
		if err != nil {
			return true, bosherr.WrapError(err, "Connecting to NATS")
		}
		return false, nil
	})

	attemptRetryStrategy := boshretry.NewAttemptRetryStrategy(natsConnectionMaxRetries, time.Second, natsRetryable, h.logger)
	err = attemptRetryStrategy.Try()
	if err != nil {
		return bosherr.WrapError(err, "Connecting")
	}

	settings := h.settingsService.GetSettings()

	subject := fmt.Sprintf("agent.%s", settings.AgentID)

	h.logger.Info(h.logTag, "Subscribing to %s", subject)

	_, err = h.client.Subscribe(subject, func(natsMsg *yagnats.Message) {
		// Do not lock handler funcs around possible network calls!
		h.handlerFuncsLock.Lock()
		handlerFuncs := h.handlerFuncs
		h.handlerFuncsLock.Unlock()

		for _, handlerFunc := range handlerFuncs {
			h.handleNatsMsg(natsMsg, handlerFunc)
		}
	})
	if err != nil {
		return bosherr.WrapErrorf(err, "Subscribing to %s", subject)
	}

	return nil
}

func (h *natsHandler) RegisterAdditionalFunc(handlerFunc boshhandler.Func) {
	// Currently not locking since RegisterAdditionalFunc
	// is not a primary way of adding handlerFunc.
	h.handlerFuncsLock.Lock()
	h.handlerFuncs = append(h.handlerFuncs, handlerFunc)
	h.handlerFuncsLock.Unlock()
}

func (h *natsHandler) Send(target boshhandler.Target, topic boshhandler.Topic, message interface{}) error {
	bytes, err := json.Marshal(message)
	if err != nil {
		return bosherr.WrapErrorf(err, "Marshalling message (target=%s, topic=%s): %#v", target, topic, message)
	}

	h.logger.Info(h.logTag, "Sending %s message '%s'", target, topic)
	h.logger.DebugWithDetails(h.logTag, "Message Payload", string(bytes))

	settings := h.settingsService.GetSettings()

	subject := fmt.Sprintf("%s.agent.%s.%s", target, topic, settings.AgentID)

	publishRetryable := boshretry.NewRetryable(func() (bool, error) {
		err := h.client.Publish(subject, bytes)
		if err != nil {
			return true, bosherr.WrapError(err, "Publishing to NATS")
		}
		return false, nil
	})
	attemptRetryStrategy := boshretry.NewAttemptRetryStrategy(3, time.Second, publishRetryable, h.logger)
	return attemptRetryStrategy.Try()
}

func (h *natsHandler) Stop() {
	h.client.Disconnect()
}

func (h *natsHandler) VerifyPeerCertificate(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	for _, chain := range verifiedChains {
		if len(chain) == 0 {
			continue
		}
		commonName := chain[0].Subject.CommonName
		match, _ := regexp.MatchString("^[a-zA-Z0-9*\\-]*.nats.bosh-internal$", commonName)
		if match {
			return nil
		}
	}
	return errors.New("Server Certificate CommonName does not match *.nats.bosh-internal")
}

func (h *natsHandler) handleNatsMsg(natsMsg *yagnats.Message, handlerFunc boshhandler.Func) {
	respBytes, req, err := boshhandler.PerformHandlerWithJSON(
		natsMsg.Payload,
		handlerFunc,
		responseMaxLength,
		h.logger,
	)

	if err != nil {
		h.logger.Error(h.logTag, "Running handler: %s", err)
		h.generateCEFLog(natsMsg, 7, err.Error())
		return
	}

	if len(respBytes) > 0 {
		err = h.client.Publish(req.ReplyTo, respBytes)
		if err != nil {
			h.generateCEFLog(natsMsg, 7, err.Error())
			h.logger.Error(h.logTag, "Publishing to the client: %s", err.Error())
			return
		}
	}

	h.generateCEFLog(natsMsg, 1, "")
}

func (h *natsHandler) runUntilInterrupted() {
	defer h.client.Disconnect()

	keepRunning := true

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	for keepRunning {
		select {
		case <-c:
			keepRunning = false
		}
	}
}

func (h *natsHandler) getConnectionInfo() (*yagnats.ConnectionInfo, error) {
	settings := h.settingsService.GetSettings()

	natsURL, err := url.Parse(settings.GetMbusURL())
	if err != nil {
		return nil, bosherr.WrapError(err, "Parsing Nats URL")
	}

	connInfo := new(yagnats.ConnectionInfo)
	connInfo.Addr = natsURL.Host

	if settings.Env.IsNATSMutualTLSEnabled() {
		connInfo.TLSInfo = &yagnats.ConnectionTLSInfo{}

		caCert := settings.Env.Bosh.Mbus.Cert.CA
		if caCert != "" {
			connInfo.TLSInfo.CertPool = x509.NewCertPool()
			if ok := connInfo.TLSInfo.CertPool.AppendCertsFromPEM([]byte(caCert)); !ok {
				return nil, bosherr.Error("Failed to load Mbus CA cert")
			}
		}

		connInfo.TLSInfo.VerifyPeerCertificate = h.VerifyPeerCertificate

		clientCertificate, err := tls.X509KeyPair([]byte(settings.Env.Bosh.Mbus.Cert.Certificate), []byte(settings.Env.Bosh.Mbus.Cert.PrivateKey))
		if err != nil {
			return nil, bosherr.WrapError(err, "Parsing certificate and private key")
		}
		connInfo.TLSInfo.ClientCert = &clientCertificate
	}

	user := natsURL.User
	if user != nil {
		password, passwordIsSet := user.Password()
		if !passwordIsSet {
			return nil, errors.New("No password set for connection")
		}
		connInfo.Password = password
		connInfo.Username = user.Username()
	}

	return connInfo, nil
}

func (h *natsHandler) generateCEFLog(natsMsg *yagnats.Message, severity int, statusReason string) {
	cef := boshhandler.NewCommonEventFormat()

	settings := h.settingsService.GetSettings()

	natsURL, err := url.Parse(settings.GetMbusURL())
	if err != nil {
		h.logger.Error(natsHandlerLogTag, err.Error())
		return
	}

	hostSplit := strings.Split(natsURL.Host, ":")
	ip := hostSplit[0]
	payload := struct {
		Method  string `json:"method"`
		ReplyTo string `json:"reply_to"`
	}{}
	err = json.Unmarshal(natsMsg.Payload, &payload)
	if err != nil {
		h.logger.Error(natsHandlerLogTag, err.Error())
	}
	cefString, err := cef.ProduceNATSRequestEventLog(ip, hostSplit[1], payload.ReplyTo, payload.Method, severity, natsMsg.Subject, statusReason)

	if err != nil {
		h.logger.Error(natsHandlerLogTag, err.Error())
		return
	}

	if severity == 7 {
		h.auditLogger.Err(cefString)
		return
	}

	h.auditLogger.Debug(cefString)
}
