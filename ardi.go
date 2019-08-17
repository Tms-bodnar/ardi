/*
ardi is a command-line tool for compiling, uploading code, and
watching logs for your usb connected arduino board. This allows you to
develop in an environment you feel comfortable in, without needing to
use arduino's web or desktop IDEs.

Usage: ardi [sketch][flags]

ardi -h --help
*/
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	arduino "github.com/arduino/arduino-cli/cli"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/tarm/serial"
)

var cli = arduino.ArduinoCli
var logger = log.New()

type targetBoardInfo struct {
	FQBN   string
	Device string
}

func filter(vs []string, f func(string) bool) []string {
	vsf := make([]string, 0)
	for _, v := range vs {
		if f(v) {
			vsf = append(vsf, v)
		}
	}
	return vsf
}

func getSketch() string {
	if len(os.Args) == 1 {
		return ""
	}

	sketch := os.Args[1]

	if !strings.Contains(sketch, "/") {
		return fmt.Sprintf("sketches/%s", sketch)
	}

	if strings.HasSuffix(sketch, "/") {
		sketch = strings.TrimSuffix(sketch, "/")
	}

	return sketch
}

func parseBaudRate(sketchPath string) int {
	var baud int
	rgx := regexp.MustCompile(`Serial\.begin\((\d+)\);`)
	sketchParts := strings.Split(sketchPath, "/")
	sketchName := sketchParts[len(sketchParts)-1]
	sketchFile := fmt.Sprintf("%s/%s.ino", sketchPath, sketchName)
	file, err := os.Open(sketchFile)
	if err != nil {
		// Log the error and return 0 for baud to let script continue
		// with either default value or value specified from command-line.
		logger.WithError(err).
			WithField("sketch", sketchPath).
			Info("Failed to read sketch")
		return baud
	}

	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		text := scanner.Text()
		if match := rgx.MatchString(text); match {
			stringBaud := strings.TrimSpace(rgx.ReplaceAllString(text, "$1"))
			if baud, err = strconv.Atoi(stringBaud); err != nil {
				// set baud to 0 and let script continue with either default
				// value or value specified from command-line.
				logger.WithError(err).Info("Failed to parse baud rate from sketch")
				baud = 0
			}
			break
		}
	}

	return baud
}

func updateCore() error {
	cli.SetArgs([]string{"core", "update-index"})
	if err := cli.Execute(); err != nil {
		return err
	}

	cli.SetArgs([]string{"core", "install", "arduino:avr"})
	if err := cli.Execute(); err != nil {
		return err
	}

	return nil
}

func getRawBoardList() (string, error) {
	out := os.Stdout
	reset := func() {
		os.Stdout = out
	}
	defer reset()

	r, w, _ := os.Pipe()
	os.Stdout = w
	buf := new(bytes.Buffer)

	cli.SetArgs([]string{"board", "list"})
	if err := cli.Execute(); err != nil {
		w.Close()
		r.Close()
		return "", err
	}

	w.Close()
	buf.ReadFrom(r)
	r.Close()

	return buf.String(), nil
}

func printFilteredBoardListWithIndices(rawBoardList string) {
	printableList := strings.SplitAfterN(rawBoardList, "\n", -1)
	printableList = filter(printableList, func(s string) bool {
		return !strings.Contains(s, "Unknown") && s != ""
	})
	for i, line := range printableList {
		if i == 0 {
			fmt.Printf("\n   %s", line)
		} else {
			fmt.Printf("%d: %s", i-1, line)
		}
	}
}

func getFilteredBoardList(rawBoardList string) []string {
	list := strings.Split(rawBoardList, "\n")
	return filter(list, func(s string) bool {
		logger.WithField("board", s).Info("Inspecting board")
		return !strings.Contains(s, "Unknown") &&
			!strings.Contains(s, "Board Name") &&
			!strings.Contains(s, "No boards found") &&
			s != ""
	})
}

func getTargetBoardInfo(filteredList []string, rawList string) (*targetBoardInfo, error) {
	var boardIndex int
	var board []string
	target := &targetBoardInfo{}
	listLength := len(filteredList)

	if listLength == 0 {
		return nil, errors.New("No boards detected")
	} else if listLength == 1 {
		boardIndex = 0
	} else {
		printFilteredBoardListWithIndices(rawList)
		fmt.Print("\nEnter number of board to upload to: ")
		if _, err := fmt.Scanf("%d", &boardIndex); err != nil {
			return nil, err
		}
	}

	if boardIndex < 0 || boardIndex > listLength-1 {
		return nil, errors.New("Invalid board selection")
	}
	board = strings.Split(filteredList[boardIndex], " ")
	target.Device = board[0]
	target.FQBN = board[len(board)-1]
	return target, nil
}

func compileAndUpload(targetBoard *targetBoardInfo, sketch string) error {
	cli.SetArgs([]string{"compile", "--fqbn", targetBoard.FQBN, sketch})
	if err := cli.Execute(); err != nil {
		return err
	}

	cli.SetArgs([]string{"upload", "-p", targetBoard.Device, "--fqbn", targetBoard.FQBN, sketch})
	if err := cli.Execute(); err != nil {
		return err
	}

	return nil
}

func watchLogs(device string, baud int) {
	logFields := log.Fields{"baud": baud, "device": device}

	config := &serial.Config{Name: device, Baud: baud}
	stream, err := serial.OpenPort(config)
	if err != nil {
		logger.WithError(err).WithFields(logFields).Fatal("Failed to read from device")
		return
	}

	for {
		var buf = make([]byte, 128)
		n, err := stream.Read(buf)
		if err != nil {
			logger.WithError(err).WithFields(logFields).Fatal("Failed to read from serial port")
		}
		fmt.Printf("%s", buf[:n])
	}

}

func process(watch bool, baud int) {
	var rawBoardList string
	var targetBoard *targetBoardInfo
	var err error
	sketch := getSketch()

	if sketch == "" {
		logger.WithError(errors.New("Missing sketch arguemnet")).Fatal("Must provide a sketch name as an argument to upload")
	}

	if watch {
		parsedBaud := parseBaudRate(sketch)

		if parsedBaud != 0 && parsedBaud != baud {
			fmt.Println("")
			logger.Infoln("Detected a different baud rate from sketch file.")
			logger.WithField("detected baud", parsedBaud).Infoln("Using detected baud rate")
			fmt.Println("")
			baud = parsedBaud
		}
	}

	logFields := log.Fields{"watch": watch, "baud": baud, "sketch": sketch}
	logWithFields := logger.WithFields(logFields)

	logWithFields.Info("Updating arduino core")
	if err = updateCore(); err != nil {
		logger.WithError(err).Fatal("Failed to update core")
	}

	logWithFields.Info("Getting board list")
	if rawBoardList, err = getRawBoardList(); err != nil {
		logger.WithError(err).Fatal("Failed to get board list")
	}

	logWithFields.Info("Filtering board list")
	filteredList := getFilteredBoardList(rawBoardList)

	logWithFields.Info("Parsing target board")
	if targetBoard, err = getTargetBoardInfo(filteredList, rawBoardList); err != nil {
		logger.WithError(err).Fatal("Failed to get target board")
	}

	logWithFields.WithField("target-board", *targetBoard).Info("Found target")
	logWithFields.Info("Compiling and uploading")
	if err := compileAndUpload(targetBoard, sketch); err != nil {
		logger.WithError(err).Fatal("Failed to compile or upload to board")
	}

	if watch {
		watchLogs(targetBoard.Device, baud)
	}
}

func main() {
	var watch bool
	var baud int
	rootCmd := &cobra.Command{
		Use:   "ardi [sketch]",
		Short: "Ardi uploads sketches and prints logs for a variety of arduino boards.",
		Long: "A light wrapper around arduino-cli that offers a quick way to upload\n" +
			"sketches and watch logs from command line for a variety of arduino boards.",
		Run: func(cmd *cobra.Command, args []string) {
			process(watch, baud)
		},
	}

	rootCmd.Flags().BoolVarP(&watch, "watch", "w", true, "watch serial port logs after uploading sketch")
	rootCmd.Flags().IntVarP(&baud, "baud", "b", 9600, "specify sketch baud rate")
	rootCmd.Execute()
}
