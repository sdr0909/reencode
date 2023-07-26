package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/schollz/progressbar/v3"
	"golang.org/x/sync/semaphore"
)

type VideoFile struct {
	path string
	name string
}

type Sizes struct {
	inSize  int64
	outSize int64
}

func main() {
	inDir := flag.String("in", "", "Input directory path")
	outDir := flag.String("out", "", "Output directory path")
	flag.Parse()

	if *inDir == "" || *outDir == "" {
		log.Fatalf("Input and output directory paths must be provided")
	}

	logFile, err := os.OpenFile("logfile.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("Failed opening log file: %v", err)
	}
	defer logFile.Close()

	log.SetOutput(logFile)

	videoFiles, err := findVideoFiles(*inDir)
	if err != nil {
		log.Fatalf("Failed to find video files: %v", err)
	}

	progressBar := progressbar.Default(int64(len(videoFiles)))

	var wg sync.WaitGroup
	sizesChan := make(chan Sizes, len(videoFiles))

	concurrency := 4
	sem := semaphore.NewWeighted(int64(concurrency))

	for _, videoFile := range videoFiles {
		wg.Add(1)
		sem.Acquire(context.Background(), 1)
		go func(videoFile VideoFile) {
			defer wg.Done()
			encodeVideoFile(videoFile, progressBar, logFile, sizesChan, *outDir)
			progressBar.Add(1)
			sem.Release(1)
		}(videoFile)
	}

	go func() {
		wg.Wait()
		close(sizesChan)
	}()

	var infileSizes []int64
	var outfileSizes []int64

	for sizes := range sizesChan {
		infileSizes = append(infileSizes, sizes.inSize)
		outfileSizes = append(outfileSizes, sizes.outSize)
	}

	inmedian := calculateMedian(infileSizes)
	outmedian := calculateMedian(outfileSizes)
	fmt.Printf("Median in file size: %.2f bytes\nMedian out file size: %.2f", float64(inmedian/8/1024/1024), float64(outmedian/8/1024/1024))

	progressBar.Finish()
}

func findVideoFiles(path string) ([]VideoFile, error) {
	var videoFiles []VideoFile

	files, err := ioutil.ReadDir(path)
	if err != nil {
		return nil, err
	}

	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".mp4") {
			videoFiles = append(videoFiles, VideoFile{path: path + "/" + file.Name(), name: file.Name()})
		}
	}

	if len(videoFiles) == 0 {
		return nil, fmt.Errorf("no video files found in the directory")
	}

	log.Printf("Found %d video(s)", len(videoFiles))

	return videoFiles, nil
}

func encodeVideoFile(videoFile VideoFile, progressBar *progressbar.ProgressBar, logFile *os.File, sizesChan chan<- Sizes, outDir string) {
	log.Printf("Starting encoding for file: %s\n", videoFile.name)

	crf := calculateCRF(videoFile.path)

	randomUUID := uuid.New().String()
	outputFile := outDir + "/" + randomUUID + ".mp4"

	if err := runFFMPEGCommand(videoFile.path, crf, outputFile); err != nil {
		log.Printf("Failed to encode file: %s, error: %v\n", videoFile.path, err)
		return
	}

	insize, outsize, err := getFileSizes(videoFile.path, outputFile)
	if err != nil {
		log.Printf("Failed to get file sizes for: %s and %s, error: %v\n", videoFile.path, outputFile, err)
		return
	}

	sizesChan <- Sizes{insize, outsize}

	progressBar.Add(1)

	writeReference(videoFile.name, outputFile)
}

func writeReference(inputName string, outputName string) {
	f, err := os.OpenFile("reference.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Println(err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(inputName + " - " + outputName + "\n"); err != nil {
		log.Println(err)
		return
	}
}

func getFileSizes(inputFile string, outputFile string) (int64, int64, error) {
	inFileInfo, err := os.Stat(inputFile)
	if err != nil {
		return 0, 0, err
	}
	outFileInfo, err := os.Stat(outputFile)
	if err != nil {
		return 0, 0, err
	}
	return inFileInfo.Size(), outFileInfo.Size(), nil
}

func runFFMPEGCommand(inputFile string, crf string, outputFile string) error {
	cmd := exec.Command("ffmpeg", "-i", inputFile, "-map", "0:v:0", "-map", "0:a:0", "-c:v", "libx265", "-b:v", "0", "-crf", crf, "-preset", "medium", "-c:a", "aac", "-b:a", "60k", "-tune", "animation", "-threads", "16", outputFile)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()

	if err != nil {
		log.Printf("ffmpeg stderr:\n%s\n", stderr.String())
		return err
	}

	return nil
}

func calculateCRF(inputFile string) string {
	inputFile = filepath.Clean(inputFile)
	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=bit_rate", "-of", "default=noprint_wrappers=1:nokey=1", inputFile)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.CombinedOutput()

	if err != nil {
		log.Printf("ffprobe stderr:\n%s\n", stderr.String())
		return "28"
	}

	bitrateStr := strings.Trim(string(output), "\n")
	bitrate, err := strconv.Atoi(bitrateStr)

	if err != nil {
		log.Println("Failed to parse video bitrate: ", err)
		return "24"
	}

	switch {
	case bitrate >= 2000000:
		return "48"
	case bitrate >= 1500000 && bitrate < 2000000:
		return "44"
	case bitrate >= 1000000 && bitrate < 1500000:
		return "32"
	case bitrate < 1000000 && bitrate > 500000:
		return "28"
	case bitrate <= 500000 && bitrate >= 200000:
		return "24"
	default:
		return "22"
	}
}
func calculateMedian(numbers []int64) int64 {
	sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })

	middle := len(numbers) / 2

	if len(numbers)%2 == 0 {
		return (numbers[middle-1] + numbers[middle]) / 2
	}

	return numbers[middle]
}
