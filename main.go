package main

import (
	"database/sql"
	"errors"
	"fmt"
	"flag"
	"bytes"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"unicode"

	_ "github.com/mattn/go-sqlite3"
)

type fileEntry struct {
	filename string
	guid     string
	kind     int
	//usedBy   map[*fileEntry]bool
	uses map[*fileEntry]bool
	inUse 	 bool
}

type addReference struct {
	source *fileEntry
	target *fileEntry
}

const (
	KindUnknown = iota
	KindAsset
	KindScene
	KindPrefab
	KindMaterial

	StateReadG = iota
	StateReadU
	StateReadI
	StateReadD
	StateReadColon
	StateSkipWhitespace
	StateReadGUID
)

var errMissingGUID = errors.New("Missing meta file")

func readGUID(filePath string) (string, error) {
	data, err := ioutil.ReadFile(filePath + ".meta")
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		parts := strings.Split(line, " ")
		if len(parts) != 2 {
			continue
		}

		option := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		if option == "guid:" {
			return value, nil
		}
	}

	return "", errMissingGUID
}

func walkAssetTree(assetPath string, doneChan chan bool) chan *fileEntry {
	results := make(chan *fileEntry)

	go func() {
		wait := sync.WaitGroup{}

		err := filepath.Walk(assetPath, func(filePath string, info os.FileInfo, err error) error {
			if err != nil {
				fmt.Printf("Failed to access path %q: %v\n", filePath, err)
				return err
			}

			wait.Add(1)
			go func() {
				defer wait.Done()

				if !info.IsDir() && path.Ext(filePath) != ".meta" {

					guid, guidErr := readGUID(filePath)
					if guidErr == nil {
						file := &fileEntry{
							filename: filePath,
							guid:     guid,
							kind:     KindUnknown,
							//usedBy:   make(map[*fileEntry]bool),
							uses: make(map[*fileEntry]bool),
						}

						switch path.Ext(filePath) {
						case ".unity":
							file.kind = KindScene
						case ".asset":
							file.kind = KindAsset
						case ".prefab":
							file.kind = KindPrefab
						case ".mat":
							file.kind = KindMaterial
						default:
							file.kind = KindUnknown
						}

						results <- file
					}

				}
			}()

			return nil
		})

		if err != nil {
			fmt.Printf("Error while collecting file information: %v\n", err)
		}

		wait.Wait()
		doneChan <- true
	}()

	return results
}

func processFile(file *fileEntry, allFiles map[string]*fileEntry, doneChan chan bool, refChan chan addReference) {
	f, err := os.Open(file.filename)
	if err != nil {
		doneChan <- true
		return
	}
	defer f.Close()

	state := StateReadG
	var partialGUID []byte = nil

	data := make([]byte, 1)
	for {
		count, err := f.Read(data)
		if count != 1 || err != nil {
			doneChan <- true
			return
		}

		ch := data[0]

		switch state {
		case StateReadG:
			if ch == 'g' {
				state = StateReadU
			}

		case StateReadU:
			if ch == 'u' {
				state = StateReadI
			} else {
				state = StateReadG
			}

		case StateReadI:
			if ch == 'i' {
				state = StateReadD
			} else {
				state = StateReadG
			}

		case StateReadD:
			if ch == 'd' {
				state = StateReadColon
			} else {
				state = StateReadG
			}

		case StateReadColon:
			if ch == ':' {
				state = StateSkipWhitespace
			} else {
				state = StateReadG
			}

		case StateSkipWhitespace:
			if ch == ' ' || ch == '\t' {
				state = StateSkipWhitespace
			} else {
				state = StateReadGUID
				partialGUID = append(partialGUID, ch)
			}

		case StateReadGUID:
			if unicode.IsNumber(rune(ch)) || unicode.IsLetter(rune(ch)) {
				partialGUID = append(partialGUID, ch)
			} else {
				if len(partialGUID) == 32 {
					fileMatch, hasMatch := allFiles[string(partialGUID)]
					if hasMatch && fileMatch != file {
						refChan <- addReference{source: file, target: fileMatch}
					}
				}

				state = StateReadG
				partialGUID = nil
			}
		}
	}
}

func processFiles(needProcessing []*fileEntry, allFiles map[string]*fileEntry) (chan addReference, chan bool) {

	results := make(chan addReference)
	done := make(chan bool)

	go func() {
		count := 0
		processDone := make(chan bool)

		for _, file := range needProcessing {
			go processFile(file, allFiles, processDone, results)
		}

		wait := true

		for wait {
			select {
			case <-processDone:
				count++
				if count >= len(needProcessing) {
					done <- true
					wait = false
				}
			}
		}

		close(processDone)
		close(results)
		close(done)
	}()

	return results, done
}

func main() {

	printReport := flag.Bool("text", false, "Generate a text report instead of an SQLite database.")
	projectPath := flag.String("project", "", "Path to project that should be scanned")
	flag.Parse()

	if *projectPath == "" {
		flag.PrintDefaults()
		//fmt.Println("Usage: AssetBrowser [-text] <asset-directory>")
		return
	}

	allFiles := make(map[string]*fileEntry)
	var needProcessing []*fileEntry

	doneChan := make(chan bool)
	assetFiles := walkAssetTree(*projectPath, doneChan)

	fmt.Println("Colleting all files...")
	collecting := true
	for collecting {
		select {
		case <-doneChan:
			collecting = false

		case file := <-assetFiles:
			if file != nil {
				allFiles[file.guid] = file
				if file.kind != KindUnknown {
					needProcessing = append(needProcessing, file)
				}
			}
		}
	}

	fmt.Println("Calculating references...")
	refs, done := processFiles(needProcessing, allFiles)
	calculating := true
	for calculating {
		select {
		case <-done:
			calculating = false
		case ref := <-refs:
			//ref.target.usedBy[ref.source] = true
			ref.source.uses[ref.target] = true
		}
	}

	fmt.Println("Generating report...")
	if *printReport {
		buf := new(bytes.Buffer)

		for _, file := range allFiles {
			file.inUse = false
		}

		for _, file := range allFiles {
			for ref := range file.uses {
				ref.inUse = true
			}
		}

		for _, file := range allFiles {
			if !file.inUse {
				buf.WriteString(file.filename)
				buf.WriteRune('\n')
			}
		}

		ioutil.WriteFile("report.txt", buf.Bytes(), 0644)

	} else {
		db, err := sql.Open("sqlite3", "report.db")
		if err != nil {
			fmt.Printf("Could not create/open database: %v\n", err)
			return
		}
		defer db.Close()

		initQueries := []string{
			`CREATE TABLE IF NOT EXISTS files (
				id INTEGER PRIMARY KEY,
				filename TEXT UNIQUE,
				guid TEXT UNIQUE);`,
			`CREATE TABLE IF NOT EXISTS links (
				id INTEGER PRIMARY KEY,
				source INTEGER NOT NULL REFERENCES files(id),
				target INTEGER NOT NULL REFERENCES files(id));`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_links ON links(source,target);`,
			`DELETE FROM files;`,
			`DELETE FROM links;`,
		}

		for _, query := range initQueries {
			_, err := db.Exec(query)
			if err != nil {
				fmt.Printf("Failed to initialize database: %v", err)
				return
			}
		}

		for _, file := range allFiles {
			_, err := db.Exec("INSERT INTO files (filename, guid) VALUES (?, ?)", file.filename, file.guid)
			if err != nil {
				fmt.Printf("Failed to insert file %s into database: %v\n", file.filename, err)
				return
			}
		}

		for _, file := range allFiles {
			for ref := range file.uses {
				_, err := db.Exec("INSERT INTO links (source, target) VALUES ((SELECT id FROM files WHERE filename = ?), (SELECT id FROM files WHERE filename = ?))", file.filename, ref.filename)
				if err != nil {
					fmt.Printf("Failed to insert link between %s and %s: %v\n", file.filename, ref.filename, err)
					return
				}
			}
		}
	}

	fmt.Println("Done")
}
