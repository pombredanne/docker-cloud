//
// Copyright (C) 2013 The Docker Cloud authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package dockercloud

import (
	"code.google.com/p/goauth2/oauth"
	compute "code.google.com/p/google-api-go-client/compute/v1"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

var (
	instanceType = flag.String("instancetype",
		"/zones/us-central1-a/machineTypes/n1-standard-1",
		"The reference to the instance type to create.")
	image = flag.String("image",
		"https://www.googleapis.com/compute/v1/projects/debian-cloud/global/images/backports-debian-7-wheezy-v20131127",
		"The GCE image to boot from.")
	diskName   = flag.String("diskname", "docker-root", "Name of the instance root disk")
	diskSizeGb = flag.Int64("disksize", 100, "Size of the root disk in GB")
)

const startup = `#!/bin/bash
sysctl -w net.ipv4.ip_forward=1
wget -qO- https://get.docker.io/ | sh
until test -f /var/run/docker.pid; do sleep 1 && echo waiting; done
grep mtu /etc/default/docker || (echo 'DOCKER_OPTS="-H :8000 -mtu 1460"' >> /etc/default/docker)
service docker restart
until echo 'GET /' >/dev/tcp/localhost/8000; do sleep 1 && echo waiting; done
`

// A Google Compute Engine implementation of the Cloud interface
type GCECloud struct {
	service   *compute.Service
	projectId string
}

// Create a GCE Cloud instance.  'clientId', 'clientSecret' and 'scope' are used to ask for a client
// credential.  'code' is optional and is only used if a cached credential can not be found.
// 'projectId' is the Google Cloud project name.
func NewCloudGce(clientId string, clientSecret string, scope string, code string, projectId string) *GCECloud {
	// Set up a configuration.
	config := &oauth.Config{
		ClientId:     clientId,
		ClientSecret: clientSecret,
		RedirectURL:  "oob",
		Scope:        scope,
		AuthURL:      "https://accounts.google.com/o/oauth2/auth",
		TokenURL:     "https://accounts.google.com/o/oauth2/token",
		// TODO(bburns) : This prob. won't work on Windows
		TokenCache: oauth.CacheFile(os.Getenv("HOME") + "/cache.json"),
		AccessType: "offline",
	}

	// Set up a Transport using the config.
	// transport := &oauth.Transport{Config: config,
	//         Transport: &LogTransport{http.DefaultTransport},}
	transport := &oauth.Transport{Config: config, Transport: http.DefaultTransport}

	// Try to pull the token from the cache; if this fails, we need to get one.
	token, err := config.TokenCache.Token()
	if err != nil {
		if clientId == "" || clientSecret == "" {
			flag.Usage()
			fmt.Fprint(os.Stderr, "Client id and secret are required.")
			os.Exit(2)
		}
		if code == "" {
			// Get an authorization code from the data provider.
			// ("Please ask the user if I can access this resource.")
			url := config.AuthCodeURL("")
			fmt.Println("Visit this URL to get a code, then run again with -code=YOUR_CODE\n")
			fmt.Println(url)
			// The below doesn't work for some reason.  Not sure why.  I get 404's
			// fmt.Print("Enter code: ")
			// bio := bufio.NewReader(os.Stdin)
			// code, err = bio.ReadString('\n')
			// if err != nil {
			//        log.Fatal("input: ", err)
			// }
			return nil
		}
		// Exchange the authorization code for an access token.
		// ("Here's the code you gave the user, now give me a token!")
		// TODO(bburns) : Put up a separate web end point to do the oauth dance, so a user can just go to a web page.
		token, err = transport.Exchange(code)
		if err != nil {
			log.Fatal("Exchange:", err)
		}
		// (The Exchange method will automatically cache the token.)
		log.Printf("Token is cached in %v", config.TokenCache)
	}

	// Make the actual request using the cached token to authenticate.
	// ("Here's the token, let me in!")
	transport.Token = token
	log.Print("refreshing token: %v", token)
	err = transport.Refresh()
	if err != nil {
		log.Fatalf("failed to refresh oauth token: %v", err)
	}
	log.Print("oauth token refreshed")

	svc, err := compute.New(transport.Client())
	if err != nil {
		log.Printf("Error creating service: %v", err)
	}
	return &GCECloud{
		service:   svc,
		projectId: projectId,
	}
}

// Implementation of the Cloud interface
func (cloud GCECloud) GetPublicIPAddress(name string, zone string) (string, error) {
	instance, err := cloud.service.Instances.Get(cloud.projectId, zone, name).Do()
	if err != nil {
		return "", err
	}
	// Found the instance, we're good.
	return instance.NetworkInterfaces[0].AccessConfigs[0].NatIP, nil
}

// Get or create a new root disk.
func (cloud GCECloud) getOrCreateRootDisk(name, zone string) (string, error) {
	log.Printf("try getting root disk: %q", name)
	disk, err := cloud.service.Disks.Get(cloud.projectId, zone, *diskName).Do()
	if err == nil {
		log.Printf("found %q", disk.SelfLink)
		return disk.SelfLink, nil
	}
	log.Printf("not found, creating root disk: %q", name)
	op, err := cloud.service.Disks.Insert(cloud.projectId, zone, &compute.Disk{
		Name: *diskName,
	}).SourceImage(*image).Do()
	if err != nil {
		log.Printf("disk insert api call failed: %v", err)
		return "", err
	}
	err = cloud.waitForOp(op, zone)
	if err != nil {
		log.Printf("disk insert operation failed: %v", err)
		return "", err
	}
	log.Printf("root disk created: %q", op.TargetLink)
	return op.TargetLink, nil
}

// Implementation of the Cloud interface
func (cloud GCECloud) CreateInstance(name string, zone string) (string, error) {
	rootDisk, err := cloud.getOrCreateRootDisk(*diskName, zone)
	if err != nil {
		log.Printf("failed to create root disk: %v", err)
		return "", err
	}
	prefix := "https://www.googleapis.com/compute/v1/projects/" + cloud.projectId
	instance := &compute.Instance{
		Name:        name,
		Description: "Docker on GCE",
		MachineType: prefix + *instanceType,
		Disks: []*compute.AttachedDisk{
			{
				Boot:   true,
				Type:   "PERSISTENT",
				Mode:   "READ_WRITE",
				Source: rootDisk,
			},
		},
		NetworkInterfaces: []*compute.NetworkInterface{
			{
				AccessConfigs: []*compute.AccessConfig{
					&compute.AccessConfig{Type: "ONE_TO_ONE_NAT"},
				},
				Network: prefix + "/global/networks/default",
			},
		},
		Metadata: &compute.Metadata{
			Items: []*compute.MetadataItems{
				{
					Key:   "startup-script",
					Value: startup,
				},
			},
		},
	}
	log.Printf("starting instance: %q", name)
	op, err := cloud.service.Instances.Insert(cloud.projectId, zone, instance).Do()
	if err != nil {
		log.Printf("instance insert api call failed: %v", err)
		return "", err
	}
	err = cloud.waitForOp(op, zone)
	if err != nil {
		log.Printf("instance insert operation failed: %v", err)
		return "", err
	}

	// Wait for docker to come up
	// TODO(bburns) : Use metadata instead to signal that docker is up and read.
	time.Sleep(60 * time.Second)

	log.Printf("instance started: %q", instance.NetworkInterfaces[0].AccessConfigs[0].NatIP)
	return instance.NetworkInterfaces[0].AccessConfigs[0].NatIP, err
}

// Implementation of the Cloud interface
func (cloud GCECloud) DeleteInstance(name string, zone string) error {
	op, err := cloud.service.Instances.Delete(cloud.projectId, zone, name).Do()
	if err != nil {
		log.Printf("Got compute.Operation, err: %#v, %v", op, err)
		return err
	}
	return cloud.waitForOp(op, zone)
}

func (cloud GCECloud) OpenSecureTunnel(name, zone string, localPort, remotePort int) (*os.Process, error) {
	return cloud.openSecureTunnel(name, zone, "localhost", localPort, remotePort)
}

func (cloud GCECloud) openSecureTunnel(name, zone, hostname string, localPort, remotePort int) (*os.Process, error) {
	ip, err := cloud.GetPublicIPAddress(name, zone)
	if err != nil {
		return nil, err
	}
	username := os.Getenv("USER")
	homedir := os.Getenv("HOME")

	sshCommand := fmt.Sprintf("-o LogLevel=quiet -o UserKnownHostsFile=/dev/null -o CheckHostIP=no -o StrictHostKeyChecking=no -i %s/.ssh/google_compute_engine -A -p 22 %s@%s -f -N -L %d:%s:%d", homedir, username, ip, localPort, hostname, remotePort)
	log.Printf("Running %s", sshCommand)
	cmd := exec.Command("ssh", strings.Split(sshCommand, " ")...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	cmd.Run()
	return cmd.Process, nil
}

// Wait for a compute operation to finish.
//   op The operation
//   zone The zone for the operation
// Returns an error if one occurs, or nil
func (cloud GCECloud) waitForOp(op *compute.Operation, zone string) error {
	op, err := cloud.service.ZoneOperations.Get(cloud.projectId, zone, op.Name).Do()
	for op.Status != "DONE" {
		time.Sleep(5 * time.Second)
		op, err = cloud.service.ZoneOperations.Get(cloud.projectId, zone, op.Name).Do()
		if err != nil {
			log.Printf("Got compute.Operation, err: %#v, %v", op, err)
		}
		if op.Status != "PENDING" && op.Status != "RUNNING" && op.Status != "DONE" {
			log.Printf("Error waiting for operation: %s\n", op)
			return errors.New(fmt.Sprintf("Bad operation: %s", op))
		}
	}
	return err
}
