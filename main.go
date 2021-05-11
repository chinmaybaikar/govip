// Copyright 2020 retinadata

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//     http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/j-keck/arping"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/clientv3/concurrency"
	"github.com/coreos/etcd/pkg/transport"
)

var (
	Version     = "Not defined"
	version     = flag.Bool("version", false, "Print version and exit")
	prefix      = flag.String("name", "/govip/", "Position to synchronize multiple govips")
	member      = flag.String("member", "hostname", "Unique name for this govip")
	vip         = flag.String("vip", "192.168.0.254/32", "VIP to announce from the selected govip")
	vif         = flag.String("vif", "eth0", "Interface to announce the VIP from")
	etcdaddress = flag.String("etcd", "https://127.0.0.1:2379", "etcd address(es)")
	cafile      = flag.String("cacert", "ca.crt", "etcd CA cert")
	certfile    = flag.String("cert", "server.crt", "etcd cert file")
	keyfile     = flag.String("key", "server.key", "etcd key file")
)

func hasIP() (bool, *netlink.Addr, netlink.Link, error) {
	vaddr, err := netlink.ParseAddr(*vip)
	if err != nil {
		return false, nil, nil, err
	}
	vlink, err := netlink.LinkByName(*vif)
	if err != nil {
		return false, nil, nil, err
	}
	addrs, err := netlink.AddrList(vlink, netlink.FAMILY_ALL)
	if err != nil {
		return false, nil, nil, err
	}

	for _, addr := range addrs {
		if vaddr.Equal(addr) {
			return true, vaddr, vlink, nil
		}
	}
	return false, vaddr, vlink, nil
}

func releaseIP() error {
	log.Debug("Releasing IP address")
	set, vaddr, vlink, err := hasIP()
	if err != nil {
		return err
	}
	if !set {
		log.Debug("IP address not found")
		return nil
	}
	if err := netlink.AddrDel(vlink, vaddr); err != nil {
		return err
	}
	log.Info("IP address released")
	return nil
}

func ensureIP() (bool, error) {
	log.Debug("Ensuring IP address")
	set, vaddr, vlink, err := hasIP()
	if err != nil {
		return false, err
	}
	if set {
		log.Debug("IP address already set")
		return false, nil
	}
	if err := netlink.AddrAdd(vlink, vaddr); err != nil {
		return false, err
	}
	log.Info("IP address set, sending gratuitous ARPs")
	for i := 0; i < 5; i++ {
		arping.GratuitousArpOverIfaceByName(vaddr.IP, *vif)
		time.Sleep(1 * time.Second)
	}

	return true, nil
}

func main() {
	flag.Parse()
	if *version {
		fmt.Println(Version)
		return
	}

	releaseIP()
	tlsInfo := transport.TLSInfo{
		CertFile:      *certfile,
		KeyFile:       *keyfile,
		TrustedCAFile: *cafile,
	}
	tlsConfig, err := tlsInfo.ClientConfig()
	if err != nil {
		log.Fatal(err)
	}
	cli, err := client.New(client.Config{
		Endpoints:   strings.Split(*etcdaddress, ","),
		DialTimeout: 5 * time.Second,
		TLS:         tlsConfig,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer cli.Close() // make sure to close the client

	quit := make(chan int)
	exit := make(chan int)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		defer func() { exit <- 0 }()
		s, err := concurrency.NewSession(cli)
		if err != nil {
			log.Fatal(err)
		}
		defer s.Close()

		e := concurrency.NewElection(s, *prefix)

		for {
			select {
			case <-time.After(5 * time.Second):
				log.Debug("Waiting to become the leader")
				err := e.Campaign(ctx, *member)
				if err == context.Canceled {
					return
				}
				if err != nil {
					log.Fatal(err)
				}
				log.Debug("I am the leader")

				res, err := ensureIP()
				if err != nil {
					log.Fatal(err)
				}
				if res {
					defer releaseIP()
				}
			case <-quit:
				return
			}
		}
	}()

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan,
		syscall.SIGINT,
		syscall.SIGTERM)

	go func() {
		for {
			s := <-signalChan
			log.Infof("Received %v", s)
			cancel()
			close(quit)
			return
		}
	}()
	code := <-exit
	cli.Close()
	log.Infof("Exiting with code: %v", code)
	os.Exit(code)
}
