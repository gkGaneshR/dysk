package client

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/Azure/azure-sdk-for-go/storage"
	"github.com/rubiojr/go-vhd/vhd"
)

const (
	deviceFile = "/dev/dysk"
	// IOCTL Command Codes
	IOCTLMOUNTDYSK   = 9901
	IOCTLUNMOUNTDYSK = 9902
	IOCTGETDYSK      = 9903
	IOCTLISTDYYSKS   = 9904
	// All in/out commands are expecting 2048 buffers.
	IOCTL_IN_OUT_MAX = 2048
)

type DyskClient interface {
	Mount(d *Dysk) error
	Unmount(name string) error
	Get(name string) (*Dysk, error)
	List() ([]*Dysk, error)
	CreatePageBlob(sizeGB uint, container string, pageBlobName string, is_vhd bool) (string, error)
}

type moduleResponse struct {
	is_error bool
	response string
}

type dyskclient struct {
	storageAccountName string
	storageAccountKey  string
	blobClient         storage.BlobStorageClient
	f                  *os.File
}

func CreateClient(account string, key string) DyskClient {
	c := dyskclient{
		storageAccountName: account,
		storageAccountKey:  key,
	}
	return &c
}

func (c *dyskclient) ensureBlobService() error {
	storageClient, err := storage.NewBasicClient(c.storageAccountName, c.storageAccountKey)
	if err != nil {
		return err
	}
	blobClient := storageClient.GetBlobService()
	c.blobClient = blobClient
	return nil
}

func (c *dyskclient) CreatePageBlob(sizeGB uint, container string, pageBlobName string, is_vhd bool) (string, error) {
	if err := c.ensureBlobService(); nil != err {
		return "", err
	}

	blobContainer := c.blobClient.GetContainerReference(container)
	sizeBytes := uint64(sizeGB * 1024 * 1024 * 1024)

	_, err := blobContainer.CreateIfNotExists(nil)
	if nil != err {
		return "", err
	}

	pageBlob := blobContainer.GetBlobReference(pageBlobName)

	pageBlob.Properties.ContentLength = int64(sizeBytes)
	err = pageBlob.PutPageBlob(nil)
	if nil != err {
		return "", err
	}

	fmt.Fprintf(os.Stderr, "Created PageBlob in account:%s %s/%s(%dGiB)\n", c.storageAccountName, container, pageBlobName, sizeGB)

	// is it vhd?
	h := vhd.CreateFixedHeader(uint64(sizeBytes), &vhd.VHDOptions{})
	b := new(bytes.Buffer)
	err = binary.Write(b, binary.BigEndian, h)
	if nil != err {
		return "", err
	}

	headerBytes := b.Bytes()
	blobRange := storage.BlobRange{
		Start: uint64(sizeBytes - uint64(len(headerBytes))),
		End:   uint64(sizeBytes - 1),
	}

	if err = pageBlob.WriteRange(blobRange, bytes.NewBuffer(headerBytes[:vhd.VHD_HEADER_SIZE]), nil); nil != err {
		return "", err
	}

	fmt.Fprintf(os.Stderr, "Wrote VHD header for PageBlob in account:%s %s/%s\n", c.storageAccountName, container, pageBlobName)

	// lease it
	leaseId, err := pageBlob.AcquireLease(-1, "", nil)
	if nil != err {
		return "", err
	}

	return leaseId, err
}

func (c *dyskclient) closeDeviceFile() error {
	if nil == c.f {
		return fmt.Errorf("Device file is not open")
	}
	return c.f.Close()
}

func (c *dyskclient) Mount(d *Dysk) error {
	if err := c.openDeviceFile(); nil != err {
		return err
	}
	defer c.closeDeviceFile()

	err := c.pre_mount(d)
	if nil != err {
		return err
	}

	as_string := dysk2string(d)
	buffer := bufferize(as_string)

	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, c.f.Fd(), IOCTLMOUNTDYSK, uintptr(unsafe.Pointer(&buffer[0])))
	if e != 0 {
		return e
	}

	res := parseResponse(buffer)
	if res.is_error {
		return fmt.Errorf(res.response)
	}

	newdysk, err := string2dysk(res.response)
	if nil != err {
		return err
	}
	d.Major = newdysk.Major
	d.Minor = newdysk.Minor
	return nil
}

func (c *dyskclient) Unmount(name string) error {
	if err := isValidDeviceName(name); nil != err {
		return err
	}

	if err := c.openDeviceFile(); nil != err {
		return err
	}
	defer c.closeDeviceFile()

	newName := fmt.Sprintf("%s\n\x00", name)
	buffer := bufferize(newName)

	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, c.f.Fd(), IOCTLUNMOUNTDYSK, uintptr(unsafe.Pointer(&buffer[0])))
	if e != 0 {
		return e
	}

	res := parseResponse(buffer)
	if res.is_error {
		return fmt.Errorf(res.response)
	}

	return nil
}

func (c *dyskclient) Get(deviceName string) (*Dysk, error) {
	if err := isValidDeviceName(deviceName); nil != err {
		return nil, err
	}

	if err := c.openDeviceFile(); nil != err {
		return nil, err
	}
	defer c.closeDeviceFile()

	d, err := c.get(deviceName)
	if nil != err {
		return nil, err
	}

	c.post_get(d)

	return d, nil
}

func (c *dyskclient) List() ([]*Dysk, error) {
	if err := c.openDeviceFile(); nil != err {
		return nil, err
	}
	defer c.closeDeviceFile()

	var dysks []*Dysk

	buffer := bufferize("-")
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, c.f.Fd(), IOCTLISTDYYSKS, uintptr(unsafe.Pointer(&buffer[0])))
	if e != 0 {
		return nil, e
	}

	res := parseResponse(buffer)
	if res.is_error {
		return nil, fmt.Errorf(res.response)
	}

	splitNames := strings.Split(res.response, "\n")
	for idx, name := range splitNames {
		if idx == (len(splitNames) - 1) {
			break
		}
		d, err := c.get(name)
		if nil != err {
			return nil, err
		}
		c.post_get(d)
		dysks = append(dysks, d)
	}

	return dysks, nil
}

// --------------------------------
// Utility Funcs
// --------------------------------
func (c *dyskclient) set_pageblob_size(d *Dysk) error {
	c.ensureBlobService()
	blobClient := c.blobClient
	containerPath := path.Dir(d.Path)
	containerPath = containerPath[1:]
	blobContainer := blobClient.GetContainerReference(containerPath)

	pageBlobName := path.Base(d.Path)
	pageBlob := blobContainer.GetBlobReference(pageBlobName)

	// Read Properties if read && is page blog then we are cool
	getProps := storage.GetBlobPropertiesOptions{
		LeaseID: d.LeaseId,
	}

	// Failed to read Properties?
	if err := pageBlob.GetProperties(&getProps); nil != err {
		return err
	}

	d.SizeGB = int(pageBlob.Properties.ContentLength / (1024 * 1024 * 1024))
	return nil
}
func (c *dyskclient) pre_mount(d *Dysk) error {
	d.AccountName = c.storageAccountName
	d.AccountKey = c.storageAccountKey

	c.set_pageblob_size(d) /* TODO: Merge size functions in one place for validation and set_pageblob_size */

	byteSize := d.SizeGB * (1024 * 1024 * 1024)
	if d.Vhd {
		byteSize -= vhd.VHD_HEADER_SIZE
	}
	d.sectorCount = uint64(byteSize / 512)
	return c.validateDysk(d)
}

func (c *dyskclient) post_get(d *Dysk) {
	// Convert sector count to size
	// check if we are VHD by measuring the difference between azure's size and disk size

	byteSize := uint64(d.sectorCount * 512)
	if d.Vhd {
		byteSize += vhd.VHD_HEADER_SIZE
	}

	d.SizeGB = int(byteSize / (1024 * 1024 * 1024))
}

func (c *dyskclient) get(deviceName string) (*Dysk, error) {
	newName := fmt.Sprintf("%s\n\x00", deviceName)
	buffer := bufferize(newName)

	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, c.f.Fd(), IOCTGETDYSK, uintptr(unsafe.Pointer(&buffer[0])))
	if e != 0 {
		return nil, e
	}

	res := parseResponse(buffer)
	if res.is_error {
		return nil, fmt.Errorf(res.response)
	}

	d, err := string2dysk(res.response)
	if nil != err {
		return nil, err
	}

	return d, nil
}

func (c *dyskclient) validateLease(d *Dysk) error {

	blobClient := c.blobClient
	containerPath := path.Dir(d.Path)
	containerPath = containerPath[1:]
	blobContainer := blobClient.GetContainerReference(containerPath)

	exists, err := blobContainer.Exists()
	if nil != err {
		return err
	}
	if !exists {
		return fmt.Errorf("Container at %s does not exist", d.Path)
	}

	pageBlobName := path.Base(d.Path)
	pageBlob := blobContainer.GetBlobReference(pageBlobName)

	exists, err = pageBlob.Exists()
	if nil != err {
		return err
	}
	if !exists {
		return fmt.Errorf("Blob at %s does not exist", d.Path)
	}

	// Read Properties if read && is page blog then we are cool
	getProps := storage.GetBlobPropertiesOptions{
		LeaseID: d.LeaseId,
	}

	// Failed to read Properties?
	if err = pageBlob.GetProperties(&getProps); nil != err {
		return err
	}

	if storage.BlobTypePage != pageBlob.Properties.BlobType {
		return fmt.Errorf("This blob is not a page blob")
	}

	//if dysk is readonly then we are done now
	if ReadOnly == d.Type {
		return nil
	}

	pageBlob.Metadata["dysk"] = "dysk" //Setting a metadata value to ensure that we have write lease
	setMetaDataProps := storage.SetBlobMetadataOptions{
		LeaseID: d.LeaseId,
	}

	if err = pageBlob.SetMetadata(&setMetaDataProps); nil != err {
		return err
	}

	return nil
}

/* TODO: use length constants */
func (c *dyskclient) validateDysk(d *Dysk) error {
	if 0 == len(d.Type) || (ReadOnly != d.Type && ReadWrite != d.Type) {
		return fmt.Errorf("Invalid type. Must be R or RW")
	}

	// lower the name
	if 0 == len(d.Name) || 32 < len(d.Name) {
		return fmt.Errorf("Invalid name. Only max of(32) chars")
	}

	if strings.Contains(d.Name, "/") || strings.Contains(d.Name, "\\") || strings.Contains(d.Name, ".") {
		return fmt.Errorf("Invalid name. Must not contain \\ / .")
	}

	if 0 == d.sectorCount {
		return fmt.Errorf("Invalid Sector count.")
	}

	if 0 == len(d.AccountName) || 256 < len(d.AccountName) {
		return fmt.Errorf("Invalid Account name. Must be <= than 256")
	}

	if 0 == len(d.AccountKey) || 128 < len(d.AccountKey) {
		return fmt.Errorf("Invalid AccountKey. Must be <= 64")
	}

	_, err := base64.StdEncoding.DecodeString(d.AccountKey)
	if nil != err {
		fmt.Errorf("Invalid account key. Must be a base64 encoded string. Error:%s", err.Error())
	}

	if 0 == len(d.Path) || 1024 < len(d.Path) {
		return fmt.Errorf("Invalid path. Must be <= 1024")
	}

	if 0 < len(d.host) && 512 < len(d.host) {
		return fmt.Errorf("Invalid host. Must be <= 512")
	} else {
		d.host = fmt.Sprintf("%s.blob.core.windows.net", d.AccountName) // Won't support sovereign clouds for now
	}

	if 0 == len(d.LeaseId) || 64 < len(d.LeaseId) {
		return fmt.Errorf("Invalid Lease Id. Must be <= 32")
	}

	addr, err := net.LookupIP(d.host)
	if nil != err {
		return fmt.Errorf("Failed to lookup ip for host:%s", d.host)
	}
	d.ip = addr[0].String()

	return c.validateLease(d)
}

// Converts a byte slice to a response object
func parseResponse(bytes []byte) *moduleResponse {
	s := string(bytes)
	firstlinebreak := strings.Index(s, "\n")
	is_error := s[:firstlinebreak] == "ERR"
	response := s[firstlinebreak+1:]

	res := &moduleResponse{
		is_error: is_error,
		response: response,
	}

	return res
}

// Converts a string to a dysk
func string2dysk(asstring string) (*Dysk, error) {
	split := strings.Split(asstring, "\n")

	sectorCount, _ := strconv.ParseUint(split[2], 10, 64)
	major, err := strconv.ParseInt(split[9], 10, 64)
	if nil != err {
		return nil, err
	}

	minor, err := strconv.ParseInt(split[10], 10, 64)
	if nil != err {
		return nil, err
	}
	is_vhd, err := strconv.ParseInt(split[11], 10, 64)

	d := Dysk{
		Type:        DyskType(split[0]),
		Name:        split[1],
		sectorCount: sectorCount,
		AccountName: split[3],
		AccountKey:  split[4],
		Path:        split[5],
		host:        split[6],
		ip:          split[7],
		LeaseId:     split[8],
		Major:       int(major),
		Minor:       int(minor),
	}
	if 1 == is_vhd {
		d.Vhd = true
	}
	return &d, nil
}

// Dysk as string
func dysk2string(d *Dysk) string {
	//type-devicename-sectorcount-accountname-accountkey-path-host-ip-lease-vhd
	const format string = "%s\n%s\n%d\n%s\n%s\n%s\n%s\n%s\n%s\n%d\n"
	is_vhd := 0
	if d.Vhd {
		is_vhd = 1
	}
	out := fmt.Sprintf(format, d.Type, d.Name, d.sectorCount, d.AccountName, d.AccountKey, d.Path, d.host, d.ip, d.LeaseId, is_vhd)
	return out
}

// string as buffer with the correct padding
func bufferize(s string) []byte {
	var b bytes.Buffer
	messageBytes := []byte(s)
	pad := make([]byte, IOCTL_IN_OUT_MAX-len(messageBytes))

	b.Write(messageBytes)
	b.Write(pad)

	return b.Bytes()
}
func (c *dyskclient) openDeviceFile() error {
	f, err := os.Open(deviceFile)
	c.f = f
	return err
}
