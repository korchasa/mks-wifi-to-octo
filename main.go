package main

import (
	"bytes"
	"fmt"
	"github.com/apsdehal/go-logger"
	"github.com/gorilla/mux"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"time"
)

var (
	MKSUploadUriPattern = "http://%s/upload?X-Filename=%s"
	printerHost = ""
	log *logger.Logger
)

func main() {
	//log.SetOutput(os.Stdout)
	//log.SetReportCaller(false)
	//log.SetLevel(log.DebugLevel)
	//log.SetFormatter(&log.TextFormatter{
	//	ForceColors: true,
	//})
	var err error
	log, err = logger.New("main", 1, os.Stdout)
	if err != nil {
		panic(fmt.Sprintf("can't init logging: %v", err))
	}

	listen := os.Getenv("LISTEN")
	if listen == "" {
		listen = "0.0.0.0:10080"
	}

	if len(os.Args) != 2 {
		log.Fatalf("Usage: %s <printer IP>", os.Args[0])
	}
	printerHost = os.Args[1]

	r := mux.NewRouter()
	r.HandleFunc("/api/version", handleVersion()).Methods("GET")
	r.HandleFunc("/api/files/local", handleFileUpload()).Methods("POST")
	log.Infof("Running on address: %s", listen)
	if err := http.ListenAndServe(listen, logMiddleware(r)); err != nil {
		panic(err)
	}
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Infof("Incoming request %s %s", r.Method, r.URL.String())
		next.ServeHTTP(w, r)
	})
}

func handleVersion() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`
			{
			  "api": "0.1",
			  "server": "1.3.10",
			  "text": "OctoPrint 1.3.10"
			}
		`))
	}
}

func handleFileUpload() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		upf, startPrinting, err := parseOctoUpload(r)
		if err != nil {
			log.Errorf("can't parse octoprint request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		uErr := uploadFileToMKS(upf)
		if uErr != nil {
			log.Errorf("can't upload file to MKS: %v", uErr)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if startPrinting {
			time.Sleep(3 * time.Second)
			printingResp, jErr := startMKSJob(upf.Filename)
			if jErr != nil {
				log.Errorf("can't start printing: %v", jErr)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			log.Infof(" from MKS: %s", printingResp)
		}

		_, _ = w.Write([]byte(`{}`))
	}
}

func parseOctoUpload(r *http.Request) (*multipart.FileHeader, bool, error) {
	parseErr := r.ParseMultipartForm(32 * 1024 * 1024)
	if parseErr != nil {
		return nil, false, fmt.Errorf("failed to parse multipart message: %v", parseErr)
	}

	startPrinting := false
	if val, ok := r.MultipartForm.Value["print"]; ok {
		if len(val) == 1 {
			if val[0] == "true" {
				startPrinting = true
			}
		}
	}

	upfs, ok := r.MultipartForm.File["file"]
	if !ok {
		return nil, false, fmt.Errorf("can't find gcode file in request")
	}
	if len(upfs) != 1 {
		return nil, false, fmt.Errorf("wrong gcode files count in request: %d", len(upfs))
	}
	upf := upfs[0]
	log.Infof("Received gcode file: filename=%s size=%d", upf.Filename, upf.Size)
	return upf, startPrinting, nil
}

func uploadFileToMKS(fileHeader *multipart.FileHeader) error {

	bodyBuf := &bytes.Buffer{}
	bodyWriter := multipart.NewWriter(bodyBuf)
	fileWriter, err := bodyWriter.CreateFormFile("uploadfile", fileHeader.Filename)
	if err != nil {
		return fmt.Errorf("error writing to buffer: %v", err)
	}
	fh, err := fileHeader.Open()
	if err != nil {
		return fmt.Errorf("can't open file %s: %v", fileHeader.Filename, err)
	}
	defer fh.Close()
	_, err = io.Copy(fileWriter, fh)
	if err != nil {
		return fmt.Errorf("can't copy from multipart to MKS request: %v", err)
	}
	bodyWriter.Close()

	uri := fmt.Sprintf(MKSUploadUriPattern, printerHost, fileHeader.Filename)
	client := &http.Client{
		Timeout: 300 * time.Second,
	}

	resp, err := client.Post(uri, "application/octet-stream", bodyBuf)
	if err != nil {
		return fmt.Errorf("error on MKS request: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("can't read MKS response body: %v", err)
	}

	log.Infof("Response from MKS [%d]: %s", resp.StatusCode, string(respBody))
	return nil
}

func startMKSJob(filename string) (string, error) {
	conn, err := net.Dial("tcp", fmt.Sprintf("%s:8080", printerHost))
	if err != nil {
		return "", fmt.Errorf("can't connect to MKS: %v", err)
	}
	defer conn.Close()

	_, _ = conn.Write([]byte(fmt.Sprintf("M23 %s\r\n", filename)))
	_, _ = conn.Write([]byte("M24\r\n"))

	buff := make([]byte, 1024)
	n, err := conn.Read(buff)
	if err != nil {
		return "", fmt.Errorf("can't read response from socket")
	}
	return string(buff[:n]), nil
}
