package client

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"github.com/deployment-io/deployment-runner-aws-controller/utils"
	"log"
	"net/rpc"
	"strings"
	"sync"
	"time"
)

type RunnerClient struct {
	sync.Mutex
	c                  *rpc.Client
	isConnected        bool
	isStarted          bool
	organizationID     string
	token              string
	currentDockerImage string
	runnerRegion       string
	awsAccountID       string
}

func getTlsClient(service, clientCertPem, clientKeyPem string) (*rpc.Client, error) {
	cert, err := tls.X509KeyPair([]byte(clientCertPem), []byte(clientKeyPem))
	if err != nil {
		log.Fatalf("client: loadkeys: %s", err)
	}
	if len(cert.Certificate) != 2 {
		log.Fatal("client.crt should have 2 concatenated certificates: client + CA")
	}
	ca, err := x509.ParseCertificate(cert.Certificate[1])
	if err != nil {
		log.Fatal(err)
	}
	certPool := x509.NewCertPool()
	certPool.AddCert(ca)
	config := tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      certPool,
	}
	conn, err := tls.Dial("tcp", service, &config)
	if err != nil {
		return nil, err
	}

	//create and connect RPC client
	return rpc.NewClient(conn), nil
}

var client = RunnerClient{}

func connect(service, organizationID, token, clientCertPem, clientKeyPem, dockerImage, region, awsAccountID string) (err error) {
	var c *rpc.Client
	if !client.isConnected {
		if len(clientCertPem) > 0 && len(clientKeyPem) > 0 {
			clientCertPem = strings.Replace(clientCertPem, "\\n", "\n", -1)
			clientKeyPem = strings.Replace(clientKeyPem, "\\n", "\n", -1)
			c, err = getTlsClient(service, clientCertPem, clientKeyPem)
		} else {
			c, err = rpc.Dial("tcp", service)
		}

		if err != nil {
			client.isConnected = false
			return err
		}

		client.c = c
		client.organizationID = organizationID
		client.token = token
		client.runnerRegion = region
		client.awsAccountID = awsAccountID
		client.currentDockerImage = dockerImage
	}

	return nil
}

var disconnectSignal = make(chan struct{})

func Connect(service, organizationID, token, clientCertPem, clientKeyPem, dockerImage, region, awsAccountID string,
	blockTillFirstConnect bool) chan struct{} {
	firstTimeConnectSignal := make(chan struct{})
	if !client.isStarted {
		client.Lock()
		defer client.Unlock()
		if !client.isStarted {
			go func() {
				firstPing := true
				for {
					select {
					case <-disconnectSignal:
						client.isStarted = false
						client.isConnected = false
						return
					default:
						isConnectedOld := client.isConnected
						if !client.isConnected {
							connect(service, organizationID, token, clientCertPem, clientKeyPem, dockerImage, region, awsAccountID)
						}
						if client.c != nil {
							runnerUpgradeDockerImage, controllerUpgradeDockerImage, upgradeFromTs, upgradeToTs, err := client.Ping(firstPing)
							if err != nil {
								client.isConnected = false
								client.c.Close()
								client.c = nil
								if isConnectedOld != client.isConnected {
									//log only when connection status changes
								}
							} else {
								firstPing = false
								client.isConnected = true
								if isConnectedOld != client.isConnected {
									if blockTillFirstConnect {
										<-firstTimeConnectSignal
										blockTillFirstConnect = false
									}
								}
								//set upgrade data
								utils.UpgradeData.Set(runnerUpgradeDockerImage, controllerUpgradeDockerImage, upgradeFromTs, upgradeToTs)
							}
						} else {
							client.isConnected = false
						}
						time.Sleep(5 * time.Second)
					}
				}
			}()
			client.isStarted = true
		}
	}
	return firstTimeConnectSignal
}

var ErrConnection = fmt.Errorf("client is not connected")

func Disconnect() error {
	if !client.isConnected {
		return ErrConnection
	}
	err := client.c.Close()
	if err != nil {
		return err
	}
	client.isConnected = false
	disconnectSignal <- struct{}{}
	return nil
}

func Get() *RunnerClient {
	return &client
}
