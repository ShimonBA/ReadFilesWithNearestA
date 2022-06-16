package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sync"

	"golang.org/x/net/html"
)

const chunkSize = 1024

// file processing statuses
const (
	statusInProcess         = iota
	statusReadingTillTheEnd = iota
	statusFinished          = iota
)

// structures used for file processing

type readingInfo struct {
	status      int
	chunkBuffer []byte
	bytesCnt    int
}

type writingInfo struct {
	chunkBuffer     []byte
	continueWriting bool
}

type processingInfo struct {
	status    int
	APosition int
}

type fileProcessing struct {
	chProcessingInfo  chan processingInfo
	chContinueReading chan bool
	wg                sync.WaitGroup
	procInfo          processingInfo
}

func newFileProcessing() *fileProcessing {
	// fileProcessing object construction

	p := new(fileProcessing)

	p.chProcessingInfo = make(chan processingInfo)
	p.chContinueReading = make(chan bool)
	p.wg.Add(1)
	p.procInfo = processingInfo{status: statusInProcess, APosition: -1}

	return p
}

func main() {
	var arrFilesProcessing []fileProcessing

	if len(os.Args) != 3 {
		fmt.Println("Usage: <source url> <target folder path>")
		return
	}

	mainURL := os.Args[1]
	folderPath := os.Args[2]

	// read the main URL and extract file URLs from it
	resp, err := http.Get(mainURL)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer resp.Body.Close()
	fileLinks := getLinks(resp.Body)

	// initialize array of file processing objects, run file processing goroutine on each file
	for _, fileURL := range fileLinks {
		fileProcessObj := newFileProcessing()
		arrFilesProcessing = append(arrFilesProcessing, *fileProcessObj)
		go processFile(&fileProcessObj.wg, fileProcessObj.chProcessingInfo, fileProcessObj.chContinueReading, fileURL, folderPath)
	}

	for {
		// infinite cycle of file processing
		activeFileProcessingFound := false
		minAPosition := -1

		for _, fileProcessObj := range arrFilesProcessing {
			if fileProcessObj.procInfo.status == statusInProcess {
				activeFileProcessingFound = true

				// try to find A character and to set the variable minAPosition accordingly
				fileProcessObj.procInfo = <-fileProcessObj.chProcessingInfo
				if fileProcessObj.procInfo.status == statusInProcess {
					if fileProcessObj.procInfo.APosition != -1 {
						if minAPosition == -1 || minAPosition > fileProcessObj.procInfo.APosition {
							minAPosition = fileProcessObj.procInfo.APosition
						}
					}
				} else if fileProcessObj.procInfo.status == statusReadingTillTheEnd {
					activeFileProcessingFound = true
				}
			}
		}

		if activeFileProcessingFound {
			// send istructions for each file processing - continue reading or stop it
			for _, fileProcessObj := range arrFilesProcessing {
				switch fileProcessObj.procInfo.status {
				case statusInProcess:
					if minAPosition != -1 && minAPosition == fileProcessObj.procInfo.APosition {
						fileProcessObj.procInfo.status = statusReadingTillTheEnd
					}
					fileProcessObj.chContinueReading <- minAPosition == -1 || minAPosition == fileProcessObj.procInfo.APosition
				case statusReadingTillTheEnd:
					fileProcessObj.chContinueReading <- true
				case statusFinished:
					fileProcessObj.wg.Wait()
					continue
				}
			}
		} else {
			fmt.Println("File processing finished")
			break
		}
	}
}

func processFile(wg *sync.WaitGroup, chProcessingInfo chan processingInfo, chContinueReading chan bool, fileURL string, folderPath string) {
	// read the file by chunks from file url and write it to file on disk
	// return status of the process and character A position to output channel
	// get instructions to continue reading or to stop it from input channel

	defer wg.Done()

	// generate filePath
	fileUrl, err := url.Parse(fileURL)
	if err != nil {
		fmt.Println(err)
		chProcessingInfo <- processingInfo{status: statusFinished, APosition: -1}
		return
	}
	filePath := folderPath + "/" + path.Base(fileUrl.Path)

	// initialize WaitGroups for reading and writing
	var wgRead, wgWrite sync.WaitGroup
	wgRead.Add(1)
	wgWrite.Add(1)

	// initialize channels
	chReadingInfo := make(chan readingInfo, 2)
	chContinueReadingFromURL := make(chan bool, 2)
	chWriteStatus := make(chan int)
	chWritingInfo := make(chan writingInfo, 2)

	go readFromURL(&wgRead, chReadingInfo, chContinueReadingFromURL, fileURL)
	go writeToDiskByChunks(&wgWrite, chWriteStatus, chWritingInfo, filePath)

	for {
		// read chunk from the reading channel
		fileReadingInfo := <-chReadingInfo
		if statusFinished == fileReadingInfo.status {
			chProcessingInfo <- processingInfo{status: statusFinished, APosition: -1}
			return
		}

		// determine position of A character
		APosition := findByteInArray(fileReadingInfo.chunkBuffer, byte('A'), fileReadingInfo.bytesCnt)

		// return position of A character and wait for instructions on further reading
		chProcessingInfo <- processingInfo{status: statusInProcess, APosition: APosition}
		continueReading := <-chContinueReading

		// write the chunk if everything is ok
		if statusFinished == <-chWriteStatus {
			chProcessingInfo <- processingInfo{status: statusFinished, APosition: -1}
			return
		}

		var bufferCopy []byte

		if continueReading {
			bufferCopy = make([]byte, fileReadingInfo.bytesCnt)
			copy(bufferCopy, fileReadingInfo.chunkBuffer)
		} else {
			bufferCopy = nil
		}

		chWritingInfo <- writingInfo{chunkBuffer: bufferCopy, continueWriting: continueReading}

		if fileReadingInfo.bytesCnt == chunkSize {
			// not EOF
			chContinueReadingFromURL <- continueReading
		} else {
			// EOF, need to finish writing
			if statusFinished == <-chWriteStatus {
				chProcessingInfo <- processingInfo{status: statusFinished, APosition: -1}
				return
			}
			chWritingInfo <- writingInfo{chunkBuffer: make([]byte, 0), continueWriting: false}
		}

		if !continueReading || fileReadingInfo.bytesCnt < chunkSize {
			// finish file processing
			wgWrite.Wait()
			if !continueReading {
				// need to delete the file
				err := os.Remove(filePath)
				if err != nil {
					fmt.Print(err)
				}
			} else {
				fmt.Println("Finished to download " + filePath)
			}
			chProcessingInfo <- processingInfo{status: statusFinished, APosition: -1}
			wgRead.Wait()
			return
		}
	}

}

func readFromURL(wg *sync.WaitGroup, chReadingInfo chan readingInfo, chContinueReadingFromURL chan bool, fileURL string) {
	// read the file by chunks from the file url
	// return the chunk buffer, count of bytes received and reading status to output channel
	// get instructions to continue reading or to stop it from input channel

	defer wg.Done()

	resp, err := http.Get(fileURL)
	if err != nil {
		fmt.Println(err)
		chReadingInfo <- readingInfo{status: statusFinished, chunkBuffer: nil, bytesCnt: 0}
		return
	}
	defer resp.Body.Close()

	buffer := make([]byte, chunkSize)

	for {
		bytesRead, err := resp.Body.Read(buffer)
		if err != nil && err != io.EOF {
			fmt.Println(err)
			chReadingInfo <- readingInfo{status: statusFinished, chunkBuffer: nil, bytesCnt: 0}
			return
		}

		chReadingInfo <- readingInfo{status: statusInProcess, chunkBuffer: buffer, bytesCnt: bytesRead}

		if err == io.EOF {
			chReadingInfo <- readingInfo{status: statusFinished, chunkBuffer: nil, bytesCnt: 0}
			return
		}

		continueReading := <-chContinueReadingFromURL
		if !continueReading {
			chReadingInfo <- readingInfo{status: statusFinished, chunkBuffer: nil, bytesCnt: 0}
			return
		}
	}
}

func writeToDiskByChunks(wg *sync.WaitGroup, chStatus chan int, chWritingInfo chan writingInfo, filePath string) {
	// write the file by chunks to disk
	// return the writing status to output channel
	// get chunk buffer and instructions to continue writing or to stop it from input channel

	defer wg.Done()

	_, err := os.Create(filePath)
	if err != nil {
		fmt.Println(err)
		chStatus <- statusFinished
		return
	}

	file, err := os.OpenFile(filePath, os.O_APPEND|os.O_WRONLY, os.ModeAppend)
	defer file.Close()

	if err != nil {
		fmt.Println(err)
		chStatus <- statusFinished
		return
	}

	for {
		chStatus <- statusInProcess
		fileWritingInfo := <-chWritingInfo
		if !fileWritingInfo.continueWriting {
			chStatus <- statusFinished
			return
		}

		_, err := file.Write(fileWritingInfo.chunkBuffer)
		if err != nil {
			fmt.Println(err)
			chStatus <- statusFinished
			return
		}

		file.Sync() //flush to disk
	}

}

func findByteInArray(a []byte, x byte, len int) int {
	for i, n := range a {
		if i >= len {
			return -1
		}
		if x == n {
			return i
		}
	}
	return -1
}

func getLinks(body io.Reader) []string {
  // get links from HTML page
  
	var links []string
	z := html.NewTokenizer(body)
	for {
		tt := z.Next()

		switch tt {
		case html.ErrorToken:
			return links
		case html.StartTagToken, html.EndTagToken:
			token := z.Token()
			if "a" == token.Data {
				for _, attr := range token.Attr {
					if attr.Key == "href" {
						links = append(links, attr.Val)
					}

				}
			}

		}
	}
}
