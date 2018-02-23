package processor

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"strconv"
	"time"

	"code.videolan.org/videolan/CrashDragon/config"
	"code.videolan.org/videolan/CrashDragon/database"
	"github.com/jinzhu/gorm"
	uuid "github.com/satori/go.uuid"
)

var rchan = make(chan database.Report, 5000)

// StartQueue runs the processor queue
func StartQueue() {
	// Spawn 4 processors
	for i := 0; i < 4; i++ {
		go processHandler()
	}
}

// AddToQueue adds new items to the queue
func AddToQueue(Report database.Report) {
	select {
	case rchan <- Report:
	default:
		log.Println("Channel full. Discarding report")
	}
}

// Reprocess is a direct way to spawn a single processor which reprocesses a single report
func Reprocess(Report database.Report) {
	processReport(Report, true)
}

// ProcessText adds the text version of the report to the database, which is only used when the text button is clicked
func ProcessText(Report *database.Report) {
	filepath := path.Join(config.C.ContentDirectory, "TXT", Report.ID.String()[0:2], Report.ID.String()[0:4])
	err := os.MkdirAll(filepath, 0755)
	if err != nil {
		return
	}
	f, err := os.Create(path.Join(filepath, Report.ID.String()+".txt"))
	if err != nil {
		return
	}
	defer f.Close()

	file := path.Join(config.C.ContentDirectory, "Reports", Report.ID.String()[0:2], Report.ID.String()[0:4], Report.ID.String()+".dmp")
	symbolsPath := path.Join(config.C.ContentDirectory, "Symfiles")

	dataTXT, err := runProcessor(file, symbolsPath, "txt")
	if err != nil {
		return
	}

	_, err = f.Write(dataTXT)
	if err != nil {
		return
	}
}

func processHandler() {
	for {
		r := <-rchan
		log.Printf("Unprocessed reports: %d", len(rchan))
		processReport(r, false)
	}
}

func runProcessor(minidumpFile string, symbolsPath string, format string) ([]byte, error) {
	cmd := exec.Command("./build/bin/minidump_stackwalk", "-f", format, minidumpFile, symbolsPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}

	data, err := ioutil.ReadAll(stdout)
	if err != nil {
		return nil, err
	}

	if err := cmd.Wait(); err != nil {
		return nil, err
	}
	return data, nil
}

func processReport(Report database.Report, reprocess bool) {
	start := time.Now()

	file := path.Join(config.C.ContentDirectory, "Reports", Report.ID.String()[0:2], Report.ID.String()[0:4], Report.ID.String()+".dmp")
	symbolsPath := path.Join(config.C.ContentDirectory, "Symfiles")

	dataJSON, err := runProcessor(file, symbolsPath, "json")
	tx := database.Db.Begin()
	if err != nil {
		os.Remove(file)
		tx.Unscoped().Delete(&Report)
		tx.Commit()
		return
	}

	Report.Report = database.ReportContent{}
	err = json.Unmarshal(dataJSON, &Report.Report)
	if err != nil {
		os.Remove(file)
		tx.Unscoped().Delete(&Report)
		tx.Commit()
		return
	}

	if Report.Report.Status != "OK" {
		Report.Processed = false
	} else {
		Report.Processed = true
	}

	Report.Os = Report.Report.SystemInfo.Os
	Report.OsVersion = Report.Report.SystemInfo.OsVer
	Report.Arch = Report.Report.SystemInfo.CPUArch

	if reprocess {
		Report.Signature = ""
		Report.CrashLocation = ""
		Report.CrashPath = ""
		Report.CrashLine = 0
	}

	for _, Frame := range Report.Report.Threads[Report.Report.CrashInfo.CrashingThread].Frames {
		if Frame.File == "" && Report.Signature != "" {
			continue
		}
		Report.Signature = Frame.Function
		if Frame.File == "" {
			continue
		}
		Report.CrashLocation = Frame.File + ":" + strconv.Itoa(Frame.Line)
		Report.CrashPath = Frame.File
		Report.CrashLine = Frame.Line
		break
	}

	var Crash database.Crash
	processCrash(tx, Report, reprocess, &Crash)
	Report.CrashID = Crash.ID

	Report.ProcessingTime = time.Since(start).Seconds()

	if reprocess {
		tx.Save(&Report)
	} else {
		created := Report.CreatedAt
		tx.Create(&Report)
		tx.Model(&Report).Update("CreatedAt", created)
	}

	tx.Commit()
}

func processCrash(tx *gorm.DB, Report database.Report, reprocess bool, Crash *database.Crash) {
	if reprocess && Report.CrashID != uuid.Nil {
		database.Db.First(&Crash, "id = ?", Report.CrashID)
		Crash.Signature = Report.Signature
	} else {
		database.Db.FirstOrInit(&Crash, "signature = ?", Report.Signature)
	}

	if Crash.ID == uuid.Nil {
		Crash.ID = uuid.NewV4()

		Crash.FirstReported = Report.CreatedAt
		Crash.Signature = Report.Signature

		Crash.AllCrashCount = 0
		Crash.WinCrashCount = 0
		Crash.MacCrashCount = 0
		Crash.LinCrashCount = 0

		Crash.ProductID = Report.ProductID
		Crash.VersionID = Report.VersionID

		Crash.Fixed = false

		tx.Create(&Crash)
		reprocess = false
	}
	if !reprocess || Report.CrashID == uuid.Nil {
		Crash.LastReported = Report.CreatedAt
		Crash.AllCrashCount++
		if Report.Os == "Windows NT" {
			Crash.WinCrashCount++
		} else if Report.Os == "Linux" {
			Crash.LinCrashCount++
		} else if Report.Os == "Mac OS X" {
			Crash.MacCrashCount++
		}
		tx.Save(&Crash)
	}
	var id uuid.UUID
	rows, err := database.Db.Model(&database.Report{}).Where("crash_id = ?", Crash.ID).Select("version_id AS vid").Group("version_id").Rows()
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			rows.Scan(&id)
			if id == Report.VersionID {
				break
			}
			tx.Model(&Crash).Set("fixed", false)
		}
	}
}
