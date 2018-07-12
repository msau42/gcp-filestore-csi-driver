/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/kubernetes/pkg/util/mount"

	csipb "github.com/container-storage-interface/spec/lib/go/csi/v0"
	cloud "sigs.k8s.io/gcp-filestore-csi-driver/pkg/cloud_provider"
	"sigs.k8s.io/gcp-filestore-csi-driver/pkg/cloud_provider/file"
	driver "sigs.k8s.io/gcp-filestore-csi-driver/pkg/csi_driver"
	"sigs.k8s.io/gcp-filestore-csi-driver/pkg/util"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const (
	endpoint       = "unix:/tmp/csi.sock"
	addr           = "/tmp/csi.sock"
	network        = "unix"
	testNamePrefix = "gcp-filestore-csi-e2e-"
	defaultPerm    = os.FileMode(0750) + os.ModeDir

	defaultSizeBytes int64 = 1 * util.Tb
	standardTier           = "STANDARD"
)

var (
	client   *csiClient
	provider *cloud.Cloud
	nodeID   string
	zone     string
	project  string

	stdVolCap = &csipb.VolumeCapability{
		AccessType: &csipb.VolumeCapability_Mount{
			Mount: &csipb.VolumeCapability_MountVolume{},
		},
		AccessMode: &csipb.VolumeCapability_AccessMode{
			Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
	stdVolCaps = []*csipb.VolumeCapability{
		stdVolCap,
	}
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Google Filestore Container Storage Interface Driver Tests")
}

var _ = BeforeSuite(func() {
	driverName := "gcp-filestore-e2e-driver"
	driverVersion := "e2e-version"
	nodeID = "gcp-filestore-csi-e2e"

	// Start the driver
	var err error
	provider, err = cloud.NewCloud(driverVersion)
	Expect(err).To(BeNil(), "Failed to get cloud provider: %v", err)
	zone = provider.Meta.GetZone()
	project = provider.Meta.GetProject()

	mounter := mount.New("")

	config := &driver.GCFSDriverConfig{
		Name:          driverName,
		Version:       driverVersion,
		NodeID:        nodeID,
		RunController: true,
		RunNode:       true,
		Mounter:       mounter,
		Cloud:         provider,
	}

	gcfsDriver, err := driver.NewGCFSDriver(config)
	Expect(err).To(BeNil(), "Failed to initialize Cloud Filestore Driver: %v", err)

	go func() {
		gcfsDriver.Run(endpoint)
	}()

	client = createCSIClient()

	// TODO: This is a hack to make sure the driver is fully up before running the tests, theres probably a better way to do this.
	time.Sleep(20 * time.Second)
})

var _ = AfterSuite(func() {
	// Close the client
	err := client.conn.Close()
	if err != nil {
		Logf("Failed to close the client")
	} else {
		Logf("Closed the client")
	}
	// TODO(dyzz): Clean up driver and other things
})

var _ = Describe("Google Cloud Filestore CSI Driver", func() {

	BeforeEach(func() {
		err := client.assertCSIConnection()
		Expect(err).To(BeNil(), "Failed to assert csi client connection: %v", err)
	})

	It("should create->mount volume and check if it is writable, then unmount->delete and check is deleted", func() {
		// Create Volume
		volName := testNamePrefix + string(uuid.NewUUID())
		vol, err := CreateVolume(volName)
		Expect(err).To(BeNil(), "CreateVolume failed with error: %v", err)
		Expect(vol).To(Not(BeNil()), "CreateVolume failed with error: %v", err)

		volId := vol.GetId()
		defer func() {
			// Delete Volume
			DeleteVolume(volId)
			Expect(err).To(BeNil(), "DeleteVolume failed")

			// Validate Volume Deleted
			instance, err := provider.File.GetInstance(
				context.Background(),
				&file.Instance{
					Project:  project,
					Location: zone,
					Name:     volName,
				})
			Expect(err).To(BeNil())
			Expect(instance).To(BeNil())
		}()

		// Validate volume created
		instance, err := provider.File.GetInstance(
			context.Background(),
			&file.Instance{
				Project:  project,
				Location: zone,
				Name:     volName,
			})
		Expect(err).To(BeNil(), "Could not get filestore instance from cloud")
		Expect(instance.Tier).To(Equal(standardTier))
		Expect(instance.Volume.Name).To(Equal("vol1"))
		Expect(instance.Volume.SizeBytes).To(Equal(defaultSizeBytes))
		Expect(instance.Name).To(Equal(volName))
		Expect(instance.Location).To(Equal(zone))
		Expect(instance.Project).To(Equal(project))

		// Mount Disk
		publishDir := filepath.Join("/tmp/", volName, "mount")
		err = os.MkdirAll(publishDir, defaultPerm)
		Expect(err).To(BeNil())
		err = NodePublishVolume(volId, publishDir, vol.GetAttributes())
		Expect(err).To(BeNil(), "NodePublishVolume failed with error")

		// Write a file
		testFile := filepath.Join(publishDir, "testfile")
		f, err := os.Create(testFile)
		Expect(err).To(BeNil(), "Opening file %s failed with error", testFile)

		testString := "test-string"
		f.WriteString(testString)
		f.Sync()
		f.Close()

		// Unmount Disk
		err = NodeUnpublishVolume(volId, publishDir)
		Expect(err).To(BeNil(), "NodeUnpublishVolume failed with error")

		// Mount disk somewhere else
		secondPublishDir := filepath.Join("/tmp/", volName, "secondmount")
		err = os.MkdirAll(secondPublishDir, defaultPerm)
		Expect(err).To(BeNil())
		err = NodePublishVolume(volId, secondPublishDir, vol.GetAttributes())
		Expect(err).To(BeNil(), "NodePublishVolume failed with error")

		// Read File
		fileContent, err := ioutil.ReadFile(filepath.Join(secondPublishDir, "testfile"))
		Expect(err).To(BeNil(), "ReadFile failed with error")
		Expect(string(fileContent)).To(Equal(testString))

		// Unmount Disk
		err = NodeUnpublishVolume(volId, secondPublishDir)
		Expect(err).To(BeNil(), "NodeUnpublishVolume failed with error")

	})
})

func Logf(format string, args ...interface{}) {
	fmt.Fprint(GinkgoWriter, args...)
}

type csiClient struct {
	conn       *grpc.ClientConn
	idClient   csipb.IdentityClient
	nodeClient csipb.NodeClient
	ctrlClient csipb.ControllerClient
}

func createCSIClient() *csiClient {
	return &csiClient{}
}

func (c *csiClient) assertCSIConnection() error {
	if c.conn == nil {
		conn, err := grpc.Dial(
			addr,
			grpc.WithInsecure(),
			grpc.WithDialer(func(target string, timeout time.Duration) (net.Conn, error) {
				return net.Dial(network, target)
			}),
		)
		if err != nil {
			return err
		}
		c.conn = conn
		c.idClient = csipb.NewIdentityClient(conn)
		c.nodeClient = csipb.NewNodeClient(conn)
		c.ctrlClient = csipb.NewControllerClient(conn)
	}
	return nil
}

func CreateVolume(volName string) (*csipb.Volume, error) {
	cvr := &csipb.CreateVolumeRequest{
		Name:               volName,
		VolumeCapabilities: stdVolCaps,
	}
	cresp, err := client.ctrlClient.CreateVolume(context.Background(), cvr)
	if err != nil {
		return nil, err
	}
	return cresp.GetVolume(), nil
}

func DeleteVolume(volId string) error {
	dvr := &csipb.DeleteVolumeRequest{
		VolumeId: volId,
	}
	_, err := client.ctrlClient.DeleteVolume(context.Background(), dvr)
	return err
}

func NodeUnpublishVolume(volumeId, publishDir string) error {
	nodeUnpublishReq := &csipb.NodeUnpublishVolumeRequest{
		VolumeId:   volumeId,
		TargetPath: publishDir,
	}
	_, err := client.nodeClient.NodeUnpublishVolume(context.Background(), nodeUnpublishReq)
	return err
}

func NodePublishVolume(volumeId, publishDir string, volumeAttrs map[string]string) error {
	nodePublishReq := &csipb.NodePublishVolumeRequest{
		VolumeId:         volumeId,
		TargetPath:       publishDir,
		VolumeCapability: stdVolCap,
		VolumeAttributes: volumeAttrs,
		Readonly:         false,
	}
	_, err := client.nodeClient.NodePublishVolume(context.Background(), nodePublishReq)
	return err
}
