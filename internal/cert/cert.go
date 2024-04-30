package cert

import (
	"crypto/tls"
	"github.com/0xJacky/Nginx-UI/internal/cert/dns"
	"github.com/0xJacky/Nginx-UI/internal/logger"
	"github.com/0xJacky/Nginx-UI/internal/nginx"
	"github.com/0xJacky/Nginx-UI/query"
	"github.com/0xJacky/Nginx-UI/settings"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge/http01"
	"github.com/go-acme/lego/v4/lego"
	legolog "github.com/go-acme/lego/v4/log"
	dnsproviders "github.com/go-acme/lego/v4/providers/dns"
	"github.com/pkg/errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	HTTP01 = "http01"
	DNS01  = "dns01"
)

func IssueCert(payload *ConfigPayload, logChan chan string, errChan chan error) {
	defer func() {
		if err := recover(); err != nil {
			logger.Error(err)
		}
	}()

	// initial a channelWriter to receive logs
	cw := NewChannelWriter()
	defer close(errChan)

	// initial a logger
	l := log.New(os.Stderr, "", log.LstdFlags)
	l.SetOutput(cw)

	// Hijack the (logger) of lego
	legolog.Logger = l

	domain := payload.ServerName

	l.Println("[INFO] [Nginx UI] Preparing lego configurations")
	user, err := payload.GetACMEUser()
	if err != nil {
		errChan <- errors.Wrap(err, "issue cert get acme user error")
		return
	}
	l.Printf("[INFO] [Nginx UI] ACME User: %s, CA Dir: %s\n", user.Email, user.CADir)

	// Start a goroutine to fetch and process logs from channel
	go func() {
		for msg := range cw.Ch {
			logChan <- string(msg)
		}
	}()

	config := lego.NewConfig(user)

	config.CADirURL = user.CADir

	// Skip TLS check
	if config.HTTPClient != nil {
		config.HTTPClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	config.Certificate.KeyType = payload.GetKeyType()

	l.Println("[INFO] [Nginx UI] Creating client facilitates communication with the CA server")
	// A client facilitates communication with the CA server.
	client, err := lego.NewClient(config)
	if err != nil {
		errChan <- errors.Wrap(err, "issue cert new client error")
		return
	}

	switch payload.ChallengeMethod {
	default:
		fallthrough
	case HTTP01:
		l.Println("[INFO] [Nginx UI] Setting HTTP01 challenge provider")
		err = client.Challenge.SetHTTP01Provider(
			http01.NewProviderServer("",
				settings.ServerSettings.HTTPChallengePort,
			),
		)
	case DNS01:
		d := query.DnsCredential
		dnsCredential, err := d.FirstByID(payload.DNSCredentialID)
		if err != nil {
			errChan <- errors.Wrap(err, "get dns credential error")
			return
		}

		l.Println("[INFO] [Nginx UI] Setting DNS01 challenge provider")
		code := dnsCredential.Config.Code
		pConfig, ok := dns.GetProvider(code)

		if !ok {
			errChan <- errors.Wrap(err, "provider not found")
		}
		l.Println("[INFO] [Nginx UI] Setting environment variables")
		if dnsCredential.Config.Configuration != nil {
			err = pConfig.SetEnv(*dnsCredential.Config.Configuration)
			if err != nil {
				break
			}
			defer func() {
				pConfig.CleanEnv()
				l.Println("[INFO] [Nginx UI] Cleaned environment variables")
			}()
			provider, err := dnsproviders.NewDNSChallengeProviderByName(code)
			if err != nil {
				break
			}
			err = client.Challenge.SetDNS01Provider(provider)
		} else {
			errChan <- errors.Wrap(err, "environment configuration is empty")
			return
		}

	}

	if err != nil {
		errChan <- errors.Wrap(err, "challenge error")
		return
	}

	request := certificate.ObtainRequest{
		Domains: domain,
		Bundle:  true,
	}

	l.Println("[INFO] [Nginx UI] Obtaining certificate")
	certificates, err := client.Certificate.Obtain(request)
	if err != nil {
		errChan <- errors.Wrap(err, "obtain certificate error")
		return
	}
	name := strings.Join(domain, "_")
	saveDir := nginx.GetConfPath("ssl/" + name + "_" + string(payload.KeyType))
	if _, err = os.Stat(saveDir); os.IsNotExist(err) {
		err = os.MkdirAll(saveDir, 0755)
		if err != nil {
			errChan <- errors.Wrap(err, "mkdir error")
			return
		}
	}

	// Each certificate comes back with the cert bytes, the bytes of the client's
	// private key, and a certificate URL. SAVE THESE TO DISK.
	l.Println("[INFO] [Nginx UI] Writing certificate to disk")
	err = os.WriteFile(filepath.Join(saveDir, "fullchain.cer"),
		certificates.Certificate, 0644)

	if err != nil {
		errChan <- errors.Wrap(err, "write fullchain.cer error")
		return
	}

	l.Println("[INFO] [Nginx UI] Writing certificate private key to disk")
	err = os.WriteFile(filepath.Join(saveDir, "private.key"),
		certificates.PrivateKey, 0644)

	if err != nil {
		errChan <- errors.Wrap(err, "write private.key error")
		return
	}

	l.Println("[INFO] [Nginx UI] Reloading nginx")

	nginx.Reload()

	l.Println("[INFO] [Nginx UI] Finished")

	// Wait log to be written
	time.Sleep(2 * time.Second)
}
