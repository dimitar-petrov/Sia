package contractmanager

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/NebulousLabs/Sia/modules"
)

// TestGrowStorageFolder checks that a storage folder can be successfully
// increased in size.
func TestGrowStorageFolder(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()
	cmt, err := newContractManagerTester("TestGrowStorageFolder")
	if err != nil {
		t.Fatal(err)
	}
	defer cmt.panicClose()

	// Add a storage folder.
	storageFolderOne := filepath.Join(cmt.persistDir, "storageFolderOne")
	// Create the storage folder dir.
	err = os.MkdirAll(storageFolderOne, 0700)
	if err != nil {
		t.Fatal(err)
	}
	err = cmt.cm.AddStorageFolder(storageFolderOne, modules.SectorSize*storageFolderGranularity)
	if err != nil {
		t.Fatal(err)
	}

	// Get the index of the storage folder.
	sfs := cmt.cm.StorageFolders()
	if len(sfs) != 1 {
		t.Fatal("there should only be one storage folder")
	}
	sfIndex := sfs[0].Index
	// Verify that the storage folder has the correct capacity.
	if sfs[0].Capacity != modules.SectorSize*storageFolderGranularity {
		t.Error("new storage folder is reporting the wrong capacity")
	}
	// Verify that the on-disk files are the right size.
	mfn := filepath.Join(storageFolderOne, metadataFile)
	sfn := filepath.Join(storageFolderOne, sectorFile)
	mfi, err := os.Stat(mfn)
	if err != nil {
		t.Fatal(err)
	}
	sfi, err := os.Stat(sfn)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(mfi.Size()) != sectorMetadataDiskSize*storageFolderGranularity {
		t.Error("metadata file is the wrong size")
	}
	if uint64(sfi.Size()) != modules.SectorSize*storageFolderGranularity {
		t.Error("sector file is the wrong size")
	}

	// Increase the size of the storage folder.
	err = cmt.cm.ResizeStorageFolder(sfIndex, modules.SectorSize*storageFolderGranularity*2)
	if err != nil {
		t.Fatal(err)
	}
	// Verify that the capacity and file sizes are correct.
	sfs = cmt.cm.StorageFolders()
	if sfs[0].Capacity != modules.SectorSize*storageFolderGranularity*2 {
		t.Error("new storage folder is reporting the wrong capacity")
	}
	mfi, err = os.Stat(mfn)
	if err != nil {
		t.Fatal(err)
	}
	sfi, err = os.Stat(sfn)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(mfi.Size()) != sectorMetadataDiskSize*storageFolderGranularity*2 {
		t.Error("metadata file is the wrong size")
	}
	if uint64(sfi.Size()) != modules.SectorSize*storageFolderGranularity*2 {
		t.Error("sector file is the wrong size")
	}

	// Restart the contract manager to see that the change is persistent.
	err = cmt.cm.Close()
	if err != nil {
		t.Fatal(err)
	}
	cmt.cm, err = New(filepath.Join(cmt.persistDir, modules.ContractManagerDir))
	if err != nil {
		t.Fatal(err)
	}

	// Verify that the capacity and file sizes are correct.
	sfs = cmt.cm.StorageFolders()
	if sfs[0].Capacity != modules.SectorSize*storageFolderGranularity*2 {
		t.Error("new storage folder is reporting the wrong capacity")
	}
	mfi, err = os.Stat(mfn)
	if err != nil {
		t.Fatal(err)
	}
	sfi, err = os.Stat(sfn)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(mfi.Size()) != sectorMetadataDiskSize*storageFolderGranularity*2 {
		t.Error("metadata file is the wrong size")
	}
	if uint64(sfi.Size()) != modules.SectorSize*storageFolderGranularity*2 {
		t.Error("sector file is the wrong size")
	}
}

// dependencyIncompleteGrow will start to have disk failures after too much
// data is written and also after 'triggered' ahs been set to true.
type dependencyIncompleteGrow struct {
	productionDependencies
	triggered bool
	mu        sync.Mutex
}

// triggerLimitFile will return an error if a call to Write is made that will
// put the total throughput of the file over 1 MiB. Counting only begins once
// triggered.
type triggerLimitFile struct {
	dig *dependencyIncompleteGrow

	throughput int
	mu         sync.Mutex
	*os.File
	sync.Mutex
}

// createFile will return a file that will return an error if a write will put
// the total throughput of the file over 1 MiB.
func (dig *dependencyIncompleteGrow) createFile(s string) (file, error) {
	osFile, err := os.Create(s)
	if err != nil {
		return nil, err
	}

	tlf := &triggerLimitFile{
		dig:  dig,
		File: osFile,
	}
	return tlf, nil
}

// Write returns an error if the operation will put the total throughput of the
// file over 8 MiB. The write will write all the way to 8 MiB before returning
// the error.
func (l *triggerLimitFile) WriteAt(b []byte, offset int64) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dig.mu.Lock()
	triggered := l.dig.triggered
	l.dig.mu.Unlock()
	if !triggered {
		return l.File.WriteAt(b, offset)
	}

	// If the limit has already been reached, return an error.
	if l.throughput >= 1<<20 {
		return 0, errors.New("triggerLimitFile throughput limit reached earlier")
	}

	// If the limit has not been reached, pass the call through to the
	// underlying file.
	if l.throughput+len(b) <= 1<<20 {
		l.throughput += len(b)
		return l.File.WriteAt(b, offset)
	}

	// If the limit has been reached, write enough bytes to get to 8 MiB, then
	// return an error.
	remaining := 1<<20 - l.throughput
	l.throughput = 1 << 20
	written, err := l.File.WriteAt(b[:remaining], offset)
	if err != nil {
		return written, err
	}
	return written, errors.New("triggerLimitFile throughput limit reached before all input was written to disk")
}

// TestGrowStorageFolderIncopmleteWrite checks that growStorageFolder operates
// as intended when the writing to increase the filesize does not complete all
// the way.
func TestGrowStorageFolderIncompleteWrite(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()
	d := new(dependencyIncompleteGrow)
	cmt, err := newMockedContractManagerTester(d, "TestGrowStorageFolderIncompleteWrite")
	if err != nil {
		t.Fatal(err)
	}
	defer cmt.panicClose()

	// Add a storage folder.
	storageFolderOne := filepath.Join(cmt.persistDir, "storageFolderOne")
	// Create the storage folder dir.
	err = os.MkdirAll(storageFolderOne, 0700)
	if err != nil {
		t.Fatal(err)
	}
	err = cmt.cm.AddStorageFolder(storageFolderOne, modules.SectorSize*storageFolderGranularity*3)
	if err != nil {
		t.Fatal(err)
	}

	// Get the index of the storage folder.
	sfs := cmt.cm.StorageFolders()
	if len(sfs) != 1 {
		t.Fatal("there should only be one storage folder")
	}
	sfIndex := sfs[0].Index
	// Verify that the storage folder has the correct capacity.
	if sfs[0].Capacity != modules.SectorSize*storageFolderGranularity*3 {
		t.Error("new storage folder is reporting the wrong capacity")
	}

	// Trigger the dependencies, so that writes begin failing.
	d.mu.Lock()
	d.triggered = true
	d.mu.Unlock()

	// Increase the size of the storage folder, to large enough that it will
	// fail.
	err = cmt.cm.ResizeStorageFolder(sfIndex, modules.SectorSize*storageFolderGranularity*25)
	if err == nil {
		t.Fatal("expecting error upon resize")
	}

	// Verify that the storage folder has the correct capacity.
	if sfs[0].Capacity != modules.SectorSize*storageFolderGranularity*3 {
		t.Error("new storage folder is reporting the wrong capacity")
	}
	// Verify that the on-disk files are the right size.
	mfn := filepath.Join(storageFolderOne, metadataFile)
	sfn := filepath.Join(storageFolderOne, sectorFile)
	mfi, err := os.Stat(mfn)
	if err != nil {
		t.Fatal(err)
	}
	sfi, err := os.Stat(sfn)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(mfi.Size()) != sectorMetadataDiskSize*storageFolderGranularity*3 {
		t.Error("metadata file is the wrong size:", mfi.Size(), sectorMetadataDiskSize*storageFolderGranularity*3)
	}
	if uint64(sfi.Size()) != modules.SectorSize*storageFolderGranularity*3 {
		t.Error("sector file is the wrong size:", sfi.Size(), modules.SectorSize*storageFolderGranularity*3)
	}

	// Restart the contract manager.
	err = cmt.cm.Close()
	if err != nil {
		t.Fatal(err)
	}
	cmt.cm, err = New(filepath.Join(cmt.persistDir, modules.ContractManagerDir))
	if err != nil {
		t.Fatal(err)
	}

	// Verify that the storage folder has the correct capacity.
	if sfs[0].Capacity != modules.SectorSize*storageFolderGranularity*3 {
		t.Error("new storage folder is reporting the wrong capacity")
	}
	// Verify that the on-disk files are the right size.
	mfi, err = os.Stat(mfn)
	if err != nil {
		t.Fatal(err)
	}
	sfi, err = os.Stat(sfn)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(mfi.Size()) != sectorMetadataDiskSize*storageFolderGranularity*3 {
		t.Error("metadata file is the wrong size:", mfi.Size(), sectorMetadataDiskSize*storageFolderGranularity*3)
	}
	if uint64(sfi.Size()) != modules.SectorSize*storageFolderGranularity*3 {
		t.Error("sector file is the wrong size:", sfi.Size(), modules.SectorSize*storageFolderGranularity*3)
	}
}

// TODO: Use an interrupt that will prevent the storage folder from completing
// the writes, simulating unclean shutdown.

// TODO: Use an interrupt that will preven the storage folder from saving all
// the way, but get it far enough that the completed post is in the WAL, such
// that recoverWAL will restore the resize as it as completed.
