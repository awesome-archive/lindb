package version

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"

	"go.uber.org/atomic"

	"github.com/lindb/lindb/kv/table"
	"github.com/lindb/lindb/pkg/bufioutil"
	"github.com/lindb/lindb/pkg/fileutil"
	"github.com/lindb/lindb/pkg/logger"
)

//go:generate mockgen -source=./version_set.go -destination=./version_set_mock.go -package=version

// StoreVersionSet maintains all metadata for kv store
type StoreVersionSet interface {
	// Recover recover version set if exist, recover been invoked when kv store init.
	Recover() error
	// Destroy closes version set, release resource, such as journal writer etc.
	Destroy() error
	// NextFileNumber generates next file number
	NextFileNumber() int64
	// ManifestFileNumber returns the current manifest file number
	ManifestFileNumber() int64
	// CommitFamilyEditLog persists edit logs to manifest file, then apply new version to family version
	CommitFamilyEditLog(family string, editLog *EditLog) error
	// CreateFamilyVersion creates family version using family name,
	// if family version exist, return exist one
	CreateFamilyVersion(family string, familyID int) FamilyVersion
	// GetFamilyVersion returns family version if exist, else return nil
	GetFamilyVersion(family string) FamilyVersion

	// newVersionID generates new version id
	newVersionID() int64
	// setNextFileNumberWithoutLock set next file number, invoker must add lock
	setNextFileNumberWithoutLock(newNextFileNumber int64)
	// numberOfLevels returns num. of levels
	numberOfLevels() int
	// getCache returns table cache for reading data
	getCache() table.Cache
}

// storeVersionSet maintains all metadata for kv store
type storeVersionSet struct {
	manifestFileNumber atomic.Int64
	nextFileNumber     atomic.Int64
	storePath          string
	familyVersions     map[string]FamilyVersion
	familyIDs          map[int]string
	versionID          atomic.Int64 // unique in for increasing version id
	storeCache         table.Cache

	numOfLevels int // num of levels

	manifest bufioutil.BufioWriter
	mutex    sync.RWMutex

	logger *logger.Logger
}

// NewStoreVersionSet new VersionSet instance
func NewStoreVersionSet(storePath string, storeCache table.Cache, numOfLevels int) StoreVersionSet {
	return &storeVersionSet{
		manifestFileNumber: *atomic.NewInt64(1), // default value for initialize store
		nextFileNumber:     *atomic.NewInt64(2), // default value
		storePath:          storePath,
		storeCache:         storeCache,
		numOfLevels:        numOfLevels,
		familyVersions:     make(map[string]FamilyVersion),
		familyIDs:          make(map[int]string),
		logger:             logger.GetLogger("kv", fmt.Sprintf("VersionSet[%s]", storePath)),
	}
}

// getCache returns table cache for reading data
func (vs *storeVersionSet) getCache() table.Cache {
	return vs.storeCache
}

// numberOfLevels returns num. of levels
func (vs *storeVersionSet) numberOfLevels() int {
	return vs.numOfLevels
}

// Destroy closes version set, release resource, such as journal writer etc.
func (vs *storeVersionSet) Destroy() error {
	vs.mutex.Lock()
	defer vs.mutex.Unlock()

	// close manifest journal writer if it exist
	if vs.manifest != nil {
		if err := vs.manifest.Close(); err != nil {
			return err
		}
	}
	return nil
}

// NextFileNumber generates next file number
func (vs *storeVersionSet) NextFileNumber() int64 {
	nextNumber := vs.nextFileNumber.Add(1)
	return nextNumber - 1
}

// ManifestFileNumber returns the current manifest file number
func (vs *storeVersionSet) ManifestFileNumber() int64 {
	return vs.manifestFileNumber.Load()
}

// CommitFamilyEditLog persists edit logs to manifest file, then apply new version to family version
func (vs *storeVersionSet) CommitFamilyEditLog(family string, editLog *EditLog) error {
	// get family version based on family name
	familyVersion := vs.GetFamilyVersion(family)
	if familyVersion == nil {
		return fmt.Errorf("cannot find family version for name: %s", family)
	}

	vs.mutex.Lock()
	defer vs.mutex.Unlock()

	// add next file number init edit log for each delta edit log
	editLog.Add(NewNextFileNumber(vs.nextFileNumber.Load()))
	// persist edit log
	if err := vs.persistEditLogs(vs.manifest, []*EditLog{editLog}); err != nil {
		return err
	}
	// get current snapshot
	snapshot := familyVersion.GetSnapshot()
	defer snapshot.Close()

	newVersion := snapshot.GetCurrent().cloneVersion()

	// apply delta edit to new version
	editLog.apply(newVersion)

	// Install the new version for family level version edit log
	familyVersion.appendVersion(newVersion)

	vs.logger.Info("log and apply new version edit", logger.Any("log", editLog))
	return nil
}

// CreateFamilyVersion creates family version using family name,
// if family version exist, return exist one
func (vs *storeVersionSet) CreateFamilyVersion(family string, familyID int) FamilyVersion {
	var familyVersion = vs.GetFamilyVersion(family)
	if familyVersion != nil {
		vs.logger.Warn("family version exist, use it.", logger.String("family", family))
		return familyVersion
	}
	familyVersion = newFamilyVersion(familyID, family, vs)
	vs.mutex.Lock()
	vs.familyVersions[family] = familyVersion
	vs.familyIDs[familyID] = family
	vs.mutex.Unlock()
	return familyVersion
}

// GetFamilyVersion returns family version if exist, else return nil
func (vs *storeVersionSet) GetFamilyVersion(family string) FamilyVersion {
	vs.mutex.RLock()
	defer vs.mutex.RUnlock()
	familyVersion, ok := vs.familyVersions[family]
	if ok {
		return familyVersion
	}
	return nil
}

// Recover recover version set if exist, recover been invoked when kv store init.
// Initialize if version file not exists, else recover old data then init journal writer.
func (vs *storeVersionSet) Recover() error {
	if !fileutil.Exist(filepath.Join(vs.storePath, current())) {
		vs.logger.Info("version set's current file not exist, initialize it")
		if err := vs.initJournal(); err != nil {
			return err
		}
		return nil
	}
	vs.logger.Info("recover version set data from journal file")
	if err := vs.recover(); err != nil {
		return err
	}
	if err := vs.initJournal(); err != nil {
		return err
	}
	return nil
}

// recover does recover logic, read journal wal record and recover it
func (vs *storeVersionSet) recover() error {
	manifestFileName, err := vs.readManifestFileName()
	if err != nil {
		return err
	}
	manifestPath := vs.getManifestFilePath(manifestFileName)
	reader, err := bufioutil.NewBufioReader(manifestPath)
	defer func() {
		if e := reader.Close(); e != nil {
			vs.logger.Error("close manifest reader error",
				logger.String("manifest", manifestPath))
		}
	}()
	if err != nil {
		return fmt.Errorf("create journal reader error:%s", err)
	}
	// read edit log
	for reader.Next() {
		record, err := reader.Read()
		if err != nil {
			return fmt.Errorf("recover data from manifest file error:%s", err)
		}
		editLog := &EditLog{}
		unmarshalErr := editLog.unmarshal(record)
		if unmarshalErr != nil {
			return fmt.Errorf("unmarshal edit log data from manifest file error:%s", unmarshalErr)
		}

		familyID := editLog.familyID
		if familyID == StoreFamilyID {
			editLog.applyVersionSet(vs)
		} else if err := vs.applyFamilyVersion(familyID, editLog); err != nil {
			return err
		}
	}
	return nil
}

// applyFamilyVersion applies edit log to family version
func (vs *storeVersionSet) applyFamilyVersion(familyID int, editLog *EditLog) error {
	// find related family version
	familyVersion := vs.getFamilyVersion(familyID)
	if familyVersion == nil {
		return fmt.Errorf("cannot get family version by id:%d", familyID)
	}
	snapshot := familyVersion.GetSnapshot()
	defer snapshot.Close()
	// apply edit log to family current family
	editLog.apply(snapshot.GetCurrent())
	return nil
}

// setNextFileNumberWithoutLock set next file number, invoker must add lock
func (vs *storeVersionSet) setNextFileNumberWithoutLock(newNextFileNumber int64) {
	vs.manifestFileNumber.Store(newNextFileNumber)
	vs.nextFileNumber.Store(newNextFileNumber + 1)
}

// readManifestFileName reads manifest file name from current file
func (vs *storeVersionSet) readManifestFileName() (string, error) {
	current := vs.getCurrentPath()
	v, err := ioutil.ReadFile(current)
	if err != nil {
		return "", fmt.Errorf("write manifest file name error:%s", err)
	}
	return string(v), nil
}

// initJournal creates journal writer,
// 1. must writes version set's data into journal,
// 2. set current manifest file name into current file.
// 3. set version set's manifest writer
func (vs *storeVersionSet) initJournal() error {
	if vs.manifest == nil {
		manifestFileName := ManifestFileName(vs.manifestFileNumber.Load()) // manifest file name
		manifestPath := vs.getManifestFilePath(manifestFileName)
		writer, err := bufioutil.NewBufioWriter(manifestPath)
		if err != nil {
			return err
		}
		// need snapshot writes snapshot first
		editLogs := vs.createSnapshot()
		if err := vs.persistEditLogs(writer, editLogs); err != nil {
			return err
		}
		// make sure write snapshot success, important!!!!!!!
		// then set manifest file name into current file
		if err := vs.setCurrent(manifestFileName); err != nil {
			return err
		}
		// finally set version set's manifest writer
		vs.manifest = writer
	}
	return nil
}

// getFamilyVersion returns family version
func (vs *storeVersionSet) getFamilyVersion(familyID int) FamilyVersion {
	vs.mutex.RLock()
	defer vs.mutex.RUnlock()
	familyName, ok := vs.familyIDs[familyID]
	if !ok {
		return nil
	}
	return vs.familyVersions[familyName]
}

// newVersionID generates new version id
func (vs *storeVersionSet) newVersionID() int64 {
	newID := vs.versionID.Add(1)
	return newID - 1
}

// setCurrent writes manifest file name into CURRENT file
func (vs *storeVersionSet) setCurrent(manifestFile string) error {
	current := vs.getCurrentPath()
	tmp := fmt.Sprintf("%s.%s", current, TmpSuffix)
	// write manifest file name into current file
	if err := ioutil.WriteFile(tmp, []byte(manifestFile), 0666); err != nil {
		return fmt.Errorf("write manifest file name into current tmp file error:%s", err)
	}
	if err := os.Rename(tmp, current); err != nil {
		return fmt.Errorf("rename current tmp file name to current error:%s", err)
	}
	return nil
}

// getCurrent returns current file path
func (vs *storeVersionSet) getCurrentPath() string {
	return filepath.Join(vs.storePath, current())
}

// getManifestFilePath returns manifest file path
func (vs *storeVersionSet) getManifestFilePath(manifestFileName string) string {
	return filepath.Join(vs.storePath, manifestFileName)
}

// createSnapshot builds current version edit log
func (vs *storeVersionSet) createSnapshot() []*EditLog {
	var editLogs []*EditLog
	// for family level edit log
	for id, name := range vs.familyIDs {
		editLog := vs.createFamilySnapshot(id, vs.familyVersions[name])
		editLogs = append(editLogs, editLog)
	}

	// for store level edit log
	editLogs = append(editLogs, vs.createStoreSnapshot())
	return editLogs
}

// createFamilySnapshot creates snapshot of edit log for family level
func (vs *storeVersionSet) createFamilySnapshot(familyID int, familyVersion FamilyVersion) *EditLog {
	editLog := NewEditLog(familyID)
	// save current version all active files
	snapshot := familyVersion.GetSnapshot()
	defer snapshot.Close()
	levels := snapshot.GetCurrent().levels
	for numOfLevel, level := range levels {
		files := level.getFiles()
		for _, file := range files {
			// level -> file meta
			newFile := CreateNewFile(int32(numOfLevel), file)
			editLog.Add(newFile)
		}
	}
	return editLog
}

// createStoreSnapshot creates snapshot of edit log for store level
func (vs *storeVersionSet) createStoreSnapshot() *EditLog {
	editLog := NewEditLog(StoreFamilyID)
	// save next file number
	editLog.Add(NewNextFileNumber(vs.nextFileNumber.Load()))
	return editLog
}

// persistEditLogs persists edit logs into manifest file
func (vs *storeVersionSet) persistEditLogs(writer bufioutil.BufioWriter, editLogs []*EditLog) error {
	for _, editLog := range editLogs {
		v, err := editLog.marshal()
		if err != nil {
			return fmt.Errorf("encode edit log error:%s", err)
		}
		if _, err := writer.Write(v); err != nil {
			return fmt.Errorf("write edit log error:%s", err)
		}
		if err := writer.Sync(); err != nil {
			return fmt.Errorf("sync edit log error:%s", err)
		}
	}
	return nil
}
