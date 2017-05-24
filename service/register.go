package service

import (
	"encoding/json"
	"fmt"
	"time"

	errgo "gopkg.in/errgo.v1"

	etcd "github.com/coreos/etcd/client"
	"golang.org/x/net/context"
)

const (
	HEARTBEAT_DURATION = 5
)

func Register(service string, host *Host, serviceInfos *Infos, stop chan struct{}) chan Credentials {
	if len(host.Name) == 0 {
		host.Name = hostname
	}
	if serviceInfos == nil {
		serviceInfos = &Infos{}
		serviceInfos.User = host.User
		serviceInfos.Password = host.Password
	}

	if len(serviceInfos.PublicHostname) == 0 && len(host.PublicHostname) != 0 {
		serviceInfos.PublicHostname = host.PublicHostname
	}

	serviceInfos.Name = service

	host.PublicHostname = serviceInfos.PublicHostname

	host.User = serviceInfos.User
	host.Password = serviceInfos.Password

	publicCredentialsChan := make(chan Credentials, 1)
	privateCredentialsChan := make(chan Credentials, 1)

	watcherStopper := make(chan struct{})

	hostKey := fmt.Sprintf("/services/%s/%s", service, host.Name)
	hostJson, _ := json.Marshal(&host)
	hostValue := string(hostJson)

	serviceKey := fmt.Sprintf("/services_infos/%s", service)
	serviceJson, _ := json.Marshal(serviceInfos)
	serviceValue := string(serviceJson)

	go func() {
		ticker := time.NewTicker((HEARTBEAT_DURATION - 1) * time.Second)

		id, err := serviceRegistration(serviceKey, serviceValue)
		for err != nil {
			id, err = serviceRegistration(serviceKey, serviceValue)
		}

		publicCredentialsChan <- Credentials{
			User:     serviceInfos.User,
			Password: serviceInfos.Password,
		}

		hostRegistration(hostKey, hostValue)

		go watch(serviceKey, id, privateCredentialsChan, watcherStopper)

		for {
			select {
			case <-stop:
				_, err := KAPI().Delete(context.Background(), hostKey, &etcd.DeleteOptions{Recursive: false})
				if err != nil {
					logger.Println("fail to remove key", hostKey)
				}
				ticker.Stop()
				watcherStopper <- struct{}{}
				return
			case credentials := <-privateCredentialsChan:
				host.User = credentials.User
				host.Password = credentials.Password
				serviceInfos.User = credentials.User
				serviceInfos.Password = credentials.Password
				publicCredentialsChan <- credentials
			case <-ticker.C:
				err := hostRegistration(hostKey, hostValue)
				// If for any random reason, there is an error,
				// we retry every second until it's ok.
				for err != nil {
					logger.Printf("lost registration of '%v': %v (%v)", service, err, Client().Endpoints())
					time.Sleep(1 * time.Second)

					err = hostRegistration(hostKey, hostValue)
					if err == nil {
						logger.Printf("recover registration of '%v'", service)
					}
				}
			}
		}
	}()

	return publicCredentialsChan
}

func watch(serviceKey string, id uint64, credentialsChan chan Credentials, stop chan struct{}) {
	done := make(chan struct{}, 1)
	ctx, cancelFunc := context.WithCancel(context.Background())
	done <- struct{}{}
	for {
		select {
		case <-stop:
			cancelFunc()
			return
		case <-done:
			go func() {
				watcher := KAPI().Watcher(serviceKey, &etcd.WatcherOptions{
					AfterIndex: id,
				})
				resp, err := watcher.Next(ctx)
				if err != nil {
					logger.Printf("lost watcher of '%v': '%v' (%v)", serviceKey, err, Client().Endpoints())
					done <- struct{}{}
					return
				}
				var serviceInfos Infos
				err = json.Unmarshal([]byte(resp.Node.Value), &serviceInfos)
				if err != nil {
					logger.Printf("error while getting service key '%v': '%v' (%v)", serviceKey, err, Client().Endpoints())
					done <- struct{}{}
					return
				}
				id = resp.Node.ModifiedIndex
				credentialsChan <- Credentials{
					User:     serviceInfos.User,
					Password: serviceInfos.Password,
				}
				done <- struct{}{}
			}()
		}
	}
}

func hostRegistration(hostKey, hostJson string) error {
	_, err := KAPI().Set(context.Background(), hostKey, hostJson, &etcd.SetOptions{TTL: HEARTBEAT_DURATION * time.Second})
	if err != nil {
		return errgo.Notef(err, "Unable to register host")
	}
	return nil

}

func serviceRegistration(serviceKey, serviceJson string) (uint64, error) {
	key, err := KAPI().Set(context.Background(), serviceKey, serviceJson, nil)
	if err != nil {
		return 0, errgo.Notef(err, "Unable to register service")
	}

	return key.Node.ModifiedIndex, nil
}
