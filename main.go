package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"

	"github.com/cenk/backoff"
	"github.com/monzo/typhon"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type ServerInfo struct {
	State string `json:"state"`
}

type Stats struct {
	CDSUpdateSuccess int `json:"cluster_manager.cds.update_success"`
	LDSUpdateSuccess int `json:"listener_manager.lds.update_success"`
}

var cdsRegex = regexp.MustCompile(`cluster_manager\.cds\.update_success: (\d)`)
var ldsRegex = regexp.MustCompile(`listener_manager\.lds\.update_success: (\d)`)

func ParseStats(bs []byte) (*Stats, error) {
	cdsMatch := cdsRegex.FindSubmatch(bs)
	if len(cdsMatch) != 2 {
		return nil, errors.New("could not match cds update success")
	}
	cds, err := strconv.Atoi(string(cdsMatch[1]))
	if err != nil {
		return nil, errors.New("could not parse cds update success as int")
	}

	ldsMatch := ldsRegex.FindSubmatch(bs)
	if len(ldsMatch) != 2 {
		return nil, errors.New("could not match lds update success")
	}
	lds, err := strconv.Atoi(string(ldsMatch[1]))
	if err != nil {
		return nil, errors.New("could not parse lds update success as int")
	}

	return &Stats{
		CDSUpdateSuccess: cds,
		LDSUpdateSuccess: lds,
	}, nil
}

func main() {
	logger := logrus.New()
	logger.Formatter = &logrus.TextFormatter{
		TimestampFormat: "2006-01-02T15:04:05.000Z07:00",
	}
	logger.Level = logrus.InfoLevel

	// Should be in format `http://127.0.0.1:9010`
	host, ok := os.LookupEnv("ENVOY_ADMIN_API")
	if ok && os.Getenv("START_WITHOUT_ENVOY") != "true" {
		logger.Infof("Blocking on an Envoy located at: %v being LIVE and receiving a LDS and CDS update", host)
		block(logger, host)
	}

	if len(os.Args) < 2 {
		return
	}

	binary, err := exec.LookPath(os.Args[1])
	if err != nil {
		logger.WithError(err).Fatal("entrypoint must be provided as an argument")
	}
	logger.Infof("Handing over to %v", binary)

	var proc *os.Process

	// Pass signals to the child process
	go func() {
		stop := make(chan os.Signal, 2)
		signal.Notify(stop)
		for sig := range stop {
			if proc != nil {
				proc.Signal(sig)
			} else {
				// Signal received before the process even started. Let's just exit.
				os.Exit(1)
			}
		}
	}()

	proc, err = os.StartProcess(binary, os.Args[1:], &os.ProcAttr{
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		panic(err)
	}

	state, err := proc.Wait()
	if err != nil {
		panic(err)
	}

	exitCode := state.ExitCode()

	switch {
	case !ok:
		// We don't have an ENVOY_ADMIN_API env var, do nothing
	case !strings.Contains(host, "127.0.0.1") && !strings.Contains(host, "localhost"):
		// Envoy is not local; do nothing
	case os.Getenv("NEVER_KILL_ENVOY") == "true":
		// We're configured never to kill envoy, do nothing
	case os.Getenv("ALWAYS_KILL_ENVOY") == "true", exitCode == 0:
		// Either we had a clean exit, or we are configured to kill envoy anyway
		url := fmt.Sprintf("%s/quitquitquit", host)

		_ = typhon.NewRequest(context.Background(), "POST", url, nil).Send().Response()
	}

	os.Exit(exitCode)
}

func block(logger *logrus.Logger, envoyHost string) {
	if os.Getenv("START_WITHOUT_ENVOY") == "true" {
		return
	}

	infoUrl := fmt.Sprintf("%s/server_info", envoyHost)

	b := backoff.NewExponentialBackOff()
	// We wait forever for envoy to start and then for a LDS and CDS push. In practice k8s will kill the pod if we take too long.
	b.MaxElapsedTime = 0

	_ = backoff.Retry(func() error {
		infoRsp := typhon.NewRequest(context.Background(), "GET", infoUrl, nil).Send().Response()

		info := &ServerInfo{}

		err := infoRsp.Decode(info)
		if err != nil {
			logger.WithError(err).Infof("Envoy at %v not reachable yet", envoyHost)
			return err
		}

		if info.State != "LIVE" {
			logger.WithError(err).Infof("Envoy at %v not LIVE", envoyHost)
			return errors.New("not live yet")
		}
		logger.Infof("Envoy at %v is LIVE", envoyHost)
		return nil
	}, b)

	statsURL := fmt.Sprintf("%s/stats", envoyHost)
	b = backoff.NewExponentialBackOff()

	// We wait forever for a LDS and CDS push. In practice k8s will kill the pod if we take too long.
	b.MaxElapsedTime = 0
	_ = backoff.Retry(func() error {
		statsRsp := typhon.NewRequest(context.Background(), "GET", statsURL, nil).Send().Response()
		statsRspBytes, _ := statsRsp.BodyBytes(false)

		stats, err := ParseStats(statsRspBytes)
		if err != nil {
			logger.WithError(err).Infof("Could not parse stats response from Envoy at %v: response: %v", envoyHost, statsRspBytes)
			return err
		}

		if stats.LDSUpdateSuccess == 0 {
			logger.Infof("Envoy at %v has not received a LDS update successfully yet", envoyHost)
			return errors.New("no LDS push yet")
		}

		if stats.CDSUpdateSuccess == 0 {
			logger.Infof("Envoy at %v has not received a CDS update successfully yet", envoyHost)
			return errors.New("no CDS push yet")
		}

		logger.Infof("Envoy at %v has received a LDS and CDS push", envoyHost)
		return nil
	}, b)
}
