package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"hash"
	"io"
	"math/big"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/AdityaAgrawal08/uplink-delta/cli/lan"
	"github.com/AdityaAgrawal08/uplink-delta/cli/pkg/crc64"
	"github.com/AdityaAgrawal08/uplink-delta/cli/pkg/tarball"
	"golang.org/x/term"
)

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

type InitRequest struct {
	Filename          string `json:"filename"`
	Size              int64  `json:"size"`
	MimeType          string `json:"mimeType"`
	HashValue         string `json:"hashValue"`
	Password          string `json:"password,omitempty"`
	ExpiresInSeconds  int    `json:"expiresInSeconds,omitempty"`
	DownloadLimit     int    `json:"downloadLimit,omitempty"`
	PartsCount        int    `json:"partsCount,omitempty"`
	ChecksumCrc64nvme string `json:"checksumCrc64nvme,omitempty"`
	IsEncrypted       bool   `json:"isEncrypted,omitempty"`
}

type InitResponse struct {
	ShareId         string   `json:"shareId"`
	UploadId        string   `json:"uploadId"`
	UploadUrl       string   `json:"uploadUrl,omitempty"`
	UploadUrls      []string `json:"uploadUrls,omitempty"`
	ObjectKey       string   `json:"objectKey"`
	Filename        string   `json:"filename"`
	StorageFilename string   `json:"storageFilename"`
	ExpiresAt       string   `json:"expiresAt"`
	UploadExpiresAt string   `json:"uploadExpiresAt"`
}

type ShareMeta struct {
	ShareId          string `json:"shareId"`
	Filename         string `json:"filename"`
	Size             int64  `json:"size"`
	MimeType         string `json:"mimeType"`
	HashValue        string `json:"hashValue"`
	ExpiresAt        string `json:"expiresAt"`
	PasswordRequired bool   `json:"passwordRequired"`
	DownloadsCount   int    `json:"downloadsCount"`
	DownloadLimit    int    `json:"downloadLimit"`
	IsEncrypted      bool   `json:"isEncrypted"`
}

type AuthorizeRequest struct {
	Password string `json:"password,omitempty"`
	Preview  bool   `json:"preview"`
}

type AuthorizeResponse struct {
	DownloadUrl string `json:"downloadUrl"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	MimeType    string `json:"mimeType"`
	HashValue   string `json:"hashValue"`
	ExpiresAt   string `json:"expiresAt"`
}

type PartInfo struct {
	PartNumber int    `json:"partNumber"`
	ETag       string `json:"etag"`
	Checksum   string `json:"checksum,omitempty"`
}

type ConfirmRequest struct {
	Parts []PartInfo `json:"parts,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cfg := LoadConfig()
	subcommand := os.Args[1]

	switch subcommand {
	case "send":
		handleSend(os.Args[2:])
	case "receive":
		handleReceive(os.Args[2:])
	case "join":
		cmdJoinChat(os.Args[2:], cfg)
	case "create":
		if len(os.Args) < 3 || os.Args[2] != "session" {
			fmt.Println("✗ Unknown command. Did you mean: uplink create session ?")
			os.Exit(1)
		}
		cmdCreateSession(os.Args[3:], cfg)
	case "help", "--help", "-h":
		printUsage()
	default:
		if strings.HasPrefix(subcommand, "-") {
			fmt.Printf("✗ Error: Unknown option \"%s\"\n\n", subcommand)
		} else {
			fmt.Printf("✗ Error: Unknown command \"%s\"\n\n", subcommand)
		}
		fmt.Println("Run:\n  uplink --help\n\nto see all available commands.")
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Uplink CLI Client (v0.0.1)")
	fmt.Println("Usage: uplink <command> [arguments] [flags]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  send        Upload a file or directory")
	fmt.Println("              uplink send report.pdf")
	fmt.Println("              uplink send folder/")
	fmt.Println()
	fmt.Println("  receive     Download a file or directory")
	fmt.Println("              uplink receive 4827165038")
	fmt.Println("              uplink receive https://uplink-delta-xi.vercel.app/share/...")
	fmt.Println()
	fmt.Println("  create session   Start a chat room and get a 6-digit key")
	fmt.Println("                   uplink create session")
	fmt.Println()
	fmt.Println("  join <key>       Join a chat room with the 6-digit key")
	fmt.Println("                   uplink join 482716")
	fmt.Println()
	fmt.Println("  help        Show available commands")
	fmt.Println("              uplink --help")
}

// normalizeFlagOrder moves all flags before the first positional argument.
//
// Go's flag package stops parsing at the first non-flag token, which silently
// drops any flags placed after a positional argument
// (e.g. "uplink send file.txt --server X" would ignore --server entirely).
func normalizeFlagOrder(args []string, valueFlags map[string]bool) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(arg) > 1 && arg[0] == '-' && arg != "-" {
			flags = append(flags, arg)
			name := strings.TrimLeft(arg, "-")
			// A "--flag value" pair consumes the next token; "--flag=value" does not.
			if !strings.Contains(name, "=") && valueFlags[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		} else {
			positional = append(positional, arg)
		}
	}
	return append(flags, positional...)
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func cleanPrintName(name string) string {
	return ansiRegex.ReplaceAllString(name, "")
}

func sanitizeFilename(name string) string {
	base := filepath.Base(name)
	base = strings.ReplaceAll(base, "/", "")
	base = strings.ReplaceAll(base, "\\", "")
	base = strings.ReplaceAll(base, "..", "")
	if base == "" || base == "." {
		return "file"
	}
	return base
}

func getServerDefault() string {
	if val := os.Getenv("UPLINK_SERVER"); val != "" {
		return strings.TrimRight(val, "/")
	}
	return "https://uplink-delta-xi.vercel.app"
}

func sanitizeServerUrl(serverUrl string) string {
	serverUrl = strings.TrimRight(serverUrl, "/")
	if !strings.HasPrefix(serverUrl, "http://") && !strings.HasPrefix(serverUrl, "https://") {
		serverUrl = "https://" + serverUrl
	} else if strings.HasPrefix(serverUrl, "http://") {
		parsed, err := url.Parse(serverUrl)
		if err != nil {
			return serverUrl
		}
		host := parsed.Hostname()
		ip := net.ParseIP(host)
		isPrivate := ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast())
		isInternalDomain := host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal")
		if !isPrivate && !isInternalDomain {
			fmt.Println("Warning: Upgrading insecure HTTP to HTTPS")
			serverUrl = "https://" + strings.TrimPrefix(serverUrl, "http://")
		}
	}
	return serverUrl
}

func parseDurationToSeconds(durationStr string) (int, error) {
	if len(durationStr) < 2 {
		return 0, fmt.Errorf("invalid duration format: must contain a number and a unit suffix (m, h, d)")
	}
	suffix := durationStr[len(durationStr)-1:]
	numStr := durationStr[:len(durationStr)-1]
	val, err := strconv.Atoi(numStr)
	if err != nil {
		return 0, fmt.Errorf("invalid number format in duration: %s", numStr)
	}
	if val <= 0 {
		return 0, fmt.Errorf("duration must be greater than zero")
	}
	switch suffix {
	case "m":
		return val * 60, nil
	case "h":
		return val * 3600, nil
	case "d":
		return val * 86400, nil
	default:
		return 0, fmt.Errorf("unknown duration suffix: %s (supported units: m, h, d)", suffix)
	}
}

func generateShareCode() string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 6)
	for i := range b {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			panic(err)
		}
		b[i] = charset[num.Int64()]
	}
	return string(b)
}

func handleSend(args []string) {
	cfg := LoadConfig()

	sendCmd := flag.NewFlagSet("send", flag.ExitOnError)
	passwordFlag := sendCmd.String("password", "", "Password to protect the share link")
	expireFlag := sendCmd.String("expire", cfg.Expiry, "Expiration duration (e.g. 5m, 30m, 2h, 1d)")
	serverFlag := sendCmd.String("server", cfg.Server, "Server base URL")
	lanFlag := sendCmd.Bool("lan", false, "Enable direct LAN P2P transfer")
	qrFlag := sendCmd.Bool("qr", false, "Force display QR code")
	noQrFlag := sendCmd.Bool("no-qr", false, "Suppress QR code display")
	encryptFlag := sendCmd.Bool("encrypt", false, "Enable Client-Side End-to-End Encryption")

	sendValueFlags := map[string]bool{
		"password": true,
		"expire":   true,
		"server":   true,
	}

	err := sendCmd.Parse(normalizeFlagOrder(args, sendValueFlags))
	if err != nil {
		fmt.Println("Error parsing flags:", err)
		os.Exit(1)
	}

	expirySeconds, err := parseDurationToSeconds(*expireFlag)
	if err != nil {
		fmt.Printf("Error parsing expire flag: %v\n", err)
		os.Exit(1)
	}

	if sendCmd.NArg() < 1 {
		fmt.Println("✗ Error: File or directory path is required.")
		fmt.Println("Usage:\n  uplink send <path>")
		os.Exit(1)
	}

	inputPath := strings.Trim(sendCmd.Arg(0), "\"'")

	fi, err := os.Stat(inputPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("✗ Error: File not found.")
			fmt.Println("Check that the path exists and that you have permission to access it.")
		} else {
			fmt.Printf("✗ Error: Accessing path failed: %v\n", err)
		}
		os.Exit(1)
	}

	maxAllowedSize := int64(200 * 1024 * 1024)
	var sizeToCheck int64
	if fi.IsDir() {
		maxAllowedSize = int64(500 * 1024 * 1024)
		var sizeErr error
		sizeToCheck, sizeErr = getDirSize(inputPath)
		if sizeErr != nil {
			fmt.Printf("✗ Error calculating directory size: %v\n", sizeErr)
			os.Exit(1)
		}
	} else {
		sizeToCheck = fi.Size()
	}

	if sizeToCheck > maxAllowedSize {
		fmt.Printf("✗ Error: Upload exceeds maximum size limit of %d MB.\n", maxAllowedSize/(1024*1024))
		os.Exit(1)
	}

	serverUrl := sanitizeServerUrl(*serverFlag)

	// Perform actual upload
	code, _, filename, _, err := performCloudUploadWrapper(context.Background(), inputPath, *passwordFlag, expirySeconds, serverUrl, *encryptFlag, *lanFlag, *qrFlag, *noQrFlag)
	if err != nil {
		fmt.Printf("\n✗ Upload failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n✓ Upload completed\n\n")
	fmt.Printf("File:\n%s\n\n", filename)
	if code != "" {
		fmt.Printf("Code:\n%s\n", code)
	}
	fmt.Printf("Expires:\n%s\n", *expireFlag)

	notifyTransferComplete(filename)
}

func performCloudUploadWrapper(ctx context.Context, inputPath string, password string, expirySeconds int, serverUrl string, isEncrypted bool, enableLan bool, qrFlag bool, noQrFlag bool) (string, string, string, int64, error) {
	cfg := LoadConfig()
	fileInfo, err := os.Stat(inputPath)
	if err != nil {
		return "", "", "", 0, err
	}

	originalName := fileInfo.Name()
	var filePath string
	var isDirectory bool

	if fileInfo.IsDir() {
		isDirectory = true
		if !strings.HasSuffix(originalName, ".tar.gz") {
			originalName = originalName + ".tar.gz"
		}
		tempFile, err := os.CreateTemp("", "uplink_tarball_*.tar.gz")
		if err != nil {
			return "", "", "", 0, err
		}
		tempFile.Close()
		filePath = tempFile.Name()
		defer os.Remove(filePath)

		outFd, err := os.OpenFile(filePath, os.O_WRONLY, 0600)
		if err != nil {
			return "", "", "", 0, err
		}
		err = tarball.Pack(inputPath, outFd)
		outFd.Close()
		if err != nil {
			return "", "", "", 0, err
		}
		fileInfo, _ = os.Stat(filePath)
	} else {
		filePath = inputPath
	}

	maxAllowedSize := int64(200 * 1024 * 1024)
	if isDirectory {
		maxAllowedSize = int64(500 * 1024 * 1024)
	}
	if fileInfo.Size() > maxAllowedSize {
		return "", "", "", 0, fmt.Errorf("upload exceeds size limit of %d MB", maxAllowedSize/(1024*1024))
	}

	// Client-side End-to-End Encryption
	var keyHex string
	if isEncrypted {
		tempEncFile, err := os.CreateTemp("", "uplink_encrypted_*.enc")
		if err != nil {
			return "", "", "", 0, fmt.Errorf("E2EE temp file creation: %w", err)
		}
		tempEncFile.Close()
		defer os.Remove(tempEncFile.Name())

		keyHex, err = EncryptFileStream(filePath, tempEncFile.Name())
		if err != nil {
			return "", "", "", 0, fmt.Errorf("E2EE encryption stream: %w", err)
		}
		filePath = tempEncFile.Name()
		fileInfo, _ = os.Stat(filePath)
	}

	file, err := os.Open(filePath)
	if err != nil {
		return "", "", "", 0, err
	}
	defer file.Close()

	// Analyze file integrity
	shaHasher := sha256.New()
	var crcHasher hash.Hash64
	var multiWriter io.Writer

	chunkSize := int64(10 * 1024 * 1024)
	if cfg.AdaptiveChunks {
		chunker := &AdaptiveChunker{}
		_, err = chunker.Measure(serverUrl)
		if err == nil {
			chunkSize = chunker.ChunkSize()
		}
	}

	isMultipart := fileInfo.Size() > chunkSize
	partsCount := 1
	if isMultipart {
		partsCount = int((fileInfo.Size() + chunkSize - 1) / chunkSize)
		crcHasher = crc64.New()
		multiWriter = io.MultiWriter(shaHasher, crcHasher)
	} else {
		multiWriter = shaHasher
	}

	_, err = io.Copy(multiWriter, file)
	if err != nil {
		return "", "", "", 0, err
	}

	hashBytes := shaHasher.Sum(nil)
	hashHex := hex.EncodeToString(hashBytes)

	var crcBase64 string
	if isMultipart {
		crcBytes := crcHasher.Sum(nil)
		crcBase64 = base64.StdEncoding.EncodeToString(crcBytes)
	}

	_, _ = file.Seek(0, 0)

	// LAN Peer mode (if enabled)
	if enableLan {
		cert, fingerprint, err := lan.GenerateEphemeralCert()
		if err == nil {
			port := cfg.LanPort
			var listener net.Listener
			var listenErr error
			for attempt := 0; attempt < 3; attempt++ {
				addr := fmt.Sprintf(":%d", port+attempt)
				listener, listenErr = net.Listen("tcp", addr)
				if listenErr == nil {
					port = port + attempt
					listener.Close()
					break
				}
			}
			if listenErr == nil {
				shareCode := generateShareCode()
				shareUrl := fmt.Sprintf("uplink receive %s --lan", shareCode)

				fmt.Printf("\n✓ Direct LAN P2P Transfer Initialized!\n")
				fmt.Printf("Share Code: %s\n", shareCode)
				fmt.Printf("Fingerprint: %s\n", fingerprint)

				showQR := false
				if qrFlag {
					showQR = true
				} else if !noQrFlag {
					showQR = ShouldShowQR(cfg.ShowQR)
				}
				if showQR {
					PrintQRCode(shareUrl)
				}

				fmt.Printf("Serving file on port %d... Waiting 1s for local peer...\n", port)
				hostname, _ := os.Hostname()
				if hostname == "" {
					hostname = "uplink-peer"
				}

				serviceInfo := lan.ServiceInfo{
					Hostname:         hostname,
					Port:             port,
					ShareCode:        shareCode,
					FileName:         originalName,
					Size:             fileInfo.Size(),
					Fingerprint:      fingerprint,
					FileSHA256:       hashHex,
					PasswordRequired: password != "",
				}

				shutdownMDNS, mdnsErr := lan.RegisterService(serviceInfo)
				if mdnsErr == nil {
					ctx, cancel := context.WithCancel(context.Background())
					serverDone := make(chan struct{})
					go func() {
						err := lan.ServeFileLAN(ctx, filePath, port, cert, shareCode, password, 1, func() {
							fmt.Println("\n✓ LAN Transfer completed successfully!")
							cancel()
							os.Exit(0)
						})
						if err != nil && err != context.Canceled {
							fmt.Printf("\n✗ LAN Server Error: %v\n", err)
						}
						close(serverDone)
					}()

					time.Sleep(1 * time.Second)
					if lan.GetActiveConnections() > 0 {
						fmt.Println("Local peer connected! Performing LAN transfer...")
						<-ctx.Done()
						shutdownMDNS()
						os.Exit(0)
					} else {
						fmt.Println("No peer connected on LAN yet. Proceeding with fallback upload to cloud...")
						cancel()
						shutdownMDNS()
					}
				}
			}
		}
	}

	// Cloud Init
	var resumeState *ResumeState
	var isResume bool
	stateFilename := sanitizeFilename(originalName) + ".json"

	var serverParts map[int]PartInfo

	if isMultipart {
		state, err := LoadResumeState(stateFilename)
		if err == nil && state.Valid() && state.SHA256 == hashHex && state.FileSize == fileInfo.Size() {
			partsUrl := fmt.Sprintf("%s/api/v1/share/%s/parts", serverUrl, state.ShareId)
			partsReq, err := http.NewRequestWithContext(ctx, "GET", partsUrl, nil)
			var partsResp *http.Response
			if err == nil {
				client := &http.Client{Timeout: 30 * time.Second}
				partsResp, err = client.Do(partsReq)
			}
			if err == nil && partsResp.StatusCode == 200 {
				var partsData struct {
					UploadId string     `json:"uploadId"`
					Parts    []PartInfo `json:"parts"`
				}
				if json.NewDecoder(partsResp.Body).Decode(&partsData) == nil {
					serverParts = make(map[int]PartInfo)
					for _, p := range partsData.Parts {
						serverParts[p.PartNumber] = p
					}
					for _, p := range state.Parts {
						serverParts[p.PartNumber] = p
					}

					var mergedParts []int
					for pNum := range serverParts {
						mergedParts = append(mergedParts, pNum)
					}

					resumeState = state
					resumeState.Done = mergedParts
					isResume = true
					fmt.Printf("Resuming upload session %s (%d/%d parts completed)...\n", state.ShareId, len(mergedParts), state.TotalParts)
				}
			}
			if partsResp != nil {
				partsResp.Body.Close()
			}
		}
	}

	var initResp InitResponse
	mimeType := mime.TypeByExtension(filepath.Ext(originalName))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	if isResume {
		initResp.ShareId = resumeState.ShareId
		initResp.UploadId = resumeState.UploadId
		initResp.UploadUrls = resumeState.UploadUrls
	} else {
		fmt.Print("Initializing upload (Pass 2/2)... ")
		initReq := InitRequest{
			Filename:         originalName,
			Size:             fileInfo.Size(),
			MimeType:         mimeType,
			HashValue:        hashHex,
			Password:         password,
			ExpiresInSeconds: expirySeconds,
			PartsCount:       partsCount,
			IsEncrypted:      isEncrypted,
		}
		if isMultipart {
			initReq.ChecksumCrc64nvme = crcBase64
		}

		jsonBytes, err := json.Marshal(initReq)
		if err != nil {
			return "", "", "", 0, err
		}

		// Generate stable idempotency key based on unique upload properties
		keyData := fmt.Sprintf("%s_%d_%s_%d_%v", hashHex, fileInfo.Size(), password, expirySeconds, isEncrypted)
		keyHash := sha256.Sum256([]byte(keyData))
		idempotencyKey := fmt.Sprintf("cli_%s", hex.EncodeToString(keyHash[:])[:24])

		initUrl := fmt.Sprintf("%s/api/v1/share/init", serverUrl)
		req, err := http.NewRequestWithContext(ctx, "POST", initUrl, bytes.NewBuffer(jsonBytes))
		if err != nil {
			return "", "", "", 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", idempotencyKey)

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return "", "", "", 0, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != 201 && resp.StatusCode != 200 {
			bodyBytes, _ := io.ReadAll(resp.Body)
			return "", "", "", 0, fmt.Errorf("status %d: %s", resp.StatusCode, string(bodyBytes))
		}

		err = json.NewDecoder(resp.Body).Decode(&initResp)
		if err != nil {
			return "", "", "", 0, err
		}
		fmt.Println("Done.")

		if isMultipart {
			resumeState = &ResumeState{
				ShareId:    initResp.ShareId,
				UploadId:   initResp.UploadId,
				UploadUrls: initResp.UploadUrls,
				FileSize:   fileInfo.Size(),
				SHA256:     hashHex,
				Done:       []int{},
				TotalParts: partsCount,
				Timestamp:  time.Now().Format(time.RFC3339),
			}
			_ = resumeState.Save(stateFilename)
		}
	}

	printer := &ProgressPrinter{
		title:      "Uploading...",
		total:      fileInfo.Size(),
		startTime:  time.Now(),
		firstPrint: true,
	}

	var confirmedParts []PartInfo
	if isMultipart {
		if serverParts == nil {
			serverParts = make(map[int]PartInfo)
		}

		completedBytes := int64(0)
		for _, p := range resumeState.Done {
			partIdx := p - 1
			pSize := chunkSize
			if int64(partIdx+1)*chunkSize > fileInfo.Size() {
				pSize = fileInfo.Size() - int64(partIdx)*chunkSize
			}
			completedBytes += pSize
		}
		printer.resumeOffset = completedBytes
		printer.resumeTime = time.Now()

		sentBytes := completedBytes // cumulative bytes already uploaded (resume-aware)

		for i := 1; i <= partsCount; i++ {
			partIdx := i - 1
			partOffset := int64(partIdx) * chunkSize
			partSize := chunkSize
			if partOffset+partSize > fileInfo.Size() {
				partSize = fileInfo.Size() - partOffset
			}

			if pInfo, exists := serverParts[i]; exists && pInfo.ETag != "" {
				confirmedParts = append(confirmedParts, pInfo)
				continue
			}

			_, err = file.Seek(partOffset, 0)
			if err != nil {
				return "", "", "", 0, err
			}

			partReader := io.LimitReader(file, partSize)
			progReader := &ProgressReader{
				reader:  partReader,
				printer: printer,
				base:    sentBytes,
			}

			uploadUrl := initResp.UploadUrls[partIdx]
			putReq, err := http.NewRequestWithContext(ctx, "PUT", uploadUrl, progReader)
			if err != nil {
				return "", "", "", 0, err
			}
			putReq.ContentLength = partSize
			putReq.Header.Set("Content-Type", "application/octet-stream")

			client := &http.Client{}
			putResp, err := client.Do(putReq)
			if err != nil {
				return "", "", "", 0, err
			}
			putResp.Body.Close()

			if putResp.StatusCode != 200 && putResp.StatusCode != 204 {
				return "", "", "", 0, fmt.Errorf("part %d upload failed: status %d", i, putResp.StatusCode)
			}

			etag := putResp.Header.Get("ETag")
			if etag == "" {
				etag = fmt.Sprintf("mock_etag_part_%d", i)
			}

			pInfo := PartInfo{
				PartNumber: i,
				ETag:       etag,
			}
			serverParts[i] = pInfo
			confirmedParts = append(confirmedParts, pInfo)

			resumeState.Done = append(resumeState.Done, i)
			resumeState.Parts = confirmedParts
			_ = resumeState.Save(stateFilename)

			sentBytes += partSize
		}

		// Force the bar to render exactly 100% — the last buffered read may
		// never trigger another Print call before EOF.
		printer.Print(fileInfo.Size())
	} else {
		_, _ = file.Seek(0, 0)
		progReader := &ProgressReader{
			reader:  file,
			printer: printer,
		}

		putReq, err := http.NewRequestWithContext(ctx, "PUT", initResp.UploadUrl, progReader)
		if err != nil {
			return "", "", "", 0, err
		}
		putReq.ContentLength = fileInfo.Size()
		putReq.Header.Set("Content-Type", mimeType)

		client := &http.Client{}
		putResp, err := client.Do(putReq)
		if err != nil {
			return "", "", "", 0, err
		}
		defer putResp.Body.Close()

		if putResp.StatusCode != 200 && putResp.StatusCode != 204 {
			bodyBytes, _ := io.ReadAll(putResp.Body)
			return "", "", "", 0, fmt.Errorf("file upload failed: status %d: %s", putResp.StatusCode, string(bodyBytes))
		}

		printer.Print(fileInfo.Size())
	}

	// Confirm Upload
	confirmUrl := fmt.Sprintf("%s/api/v1/share/%s/confirm", serverUrl, initResp.ShareId)
	confirmReq := ConfirmRequest{}
	if isMultipart {
		confirmReq.Parts = confirmedParts
	}

	confirmJson, err := json.Marshal(confirmReq)
	if err != nil {
		return "", "", "", 0, err
	}

	postReq, err := http.NewRequestWithContext(ctx, "POST", confirmUrl, bytes.NewBuffer(confirmJson))
	if err != nil {
		return "", "", "", 0, err
	}
	postReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	confirmResp, err := client.Do(postReq)
	if err != nil {
		return "", "", "", 0, err
	}
	defer confirmResp.Body.Close()

	if confirmResp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(confirmResp.Body)
		return "", "", "", 0, fmt.Errorf("confirmation failed: status %d: %s", confirmResp.StatusCode, string(bodyBytes))
	}

	var confirmData struct {
		Message      string `json:"message"`
		ShareId      string `json:"shareId"`
		DownloadCode string `json:"downloadCode"`
		Status       string `json:"status"`
	}
	_ = json.NewDecoder(confirmResp.Body).Decode(&confirmData)

	if isMultipart {
		_ = DeleteResumeState(stateFilename)
	}

	shareLink := fmt.Sprintf("%s/share/%s", serverUrl, initResp.ShareId)
	if isEncrypted && keyHex != "" {
		shareLink = fmt.Sprintf("%s:%s", shareLink, keyHex)
	}

	displayCode := confirmData.DownloadCode
	if isEncrypted && keyHex != "" {
		displayCode = fmt.Sprintf("%s:%s", confirmData.DownloadCode, keyHex)
	}

	return displayCode, shareLink, originalName, fileInfo.Size(), nil
}

func handleReceive(args []string) {
	cfg := LoadConfig()

	recvCmd := flag.NewFlagSet("receive", flag.ExitOnError)
	forceFlag := recvCmd.Bool("force", false, "Force overwrite if file/directory exists")
	forceShortFlag := recvCmd.Bool("f", false, "Force overwrite (shortcut)")
	renameFlag := recvCmd.Bool("rename", false, "Rename downloaded target if it exists")
	renameShortFlag := recvCmd.Bool("r", false, "Rename downloaded target (shortcut)")
	passwordFlag := recvCmd.String("password", "", "Decryption password if protected")
	serverFlag := recvCmd.String("server", cfg.Server, "Server base URL")
	mkdirFlag := recvCmd.Bool("mkdir", false, "Create destination directory if it doesn't exist")
	mkdirShortFlag := recvCmd.Bool("p", false, "Create destination directory (shortcut)")
	lanFlag := recvCmd.Bool("lan", false, "Enable direct LAN P2P transfer")

	receiveValueFlags := map[string]bool{
		"password": true,
		"server":   true,
	}

	err := recvCmd.Parse(normalizeFlagOrder(args, receiveValueFlags))
	if err != nil {
		fmt.Println("Error parsing flags:", err)
		os.Exit(1)
	}

	if recvCmd.NArg() < 1 {
		fmt.Println("✗ Error: Share link, ID, or download code is required.")
		fmt.Println("Usage:\n  uplink receive <share-link-or-code> [dest]")
		os.Exit(1)
	}

	shareInput := recvCmd.Arg(0)
	destPath := ""
	if recvCmd.NArg() >= 2 {
		destPath = recvCmd.Arg(1)
	}

	// Client-side E2EE decryption key check
	keyHex := ""
	if idx := strings.Index(shareInput, ":"); idx != -1 {
		keyHex = shareInput[idx+1:]
		shareInput = shareInput[:idx]
	}

	shareId := shareInput
	serverUrl := *serverFlag
	if serverUrl == "" {
		serverUrl = cfg.Server
	}

	isShortCode, _ := regexp.MatchString(`^\d{10}$`, shareInput)
	if isShortCode {
		shareId = shareInput
	} else if strings.Contains(shareInput, "/share/") {
		u, err := url.Parse(shareInput)
		if err == nil && u.Host != "" {
			serverUrl = fmt.Sprintf("%s://%s", u.Scheme, u.Host)
			pathParts := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(pathParts) > 0 {
				shareId = pathParts[len(pathParts)-1]
			}
		}
	} else if strings.Contains(shareInput, "/") {
		parts := strings.Split(shareInput, "/")
		shareId = parts[len(parts)-1]
		host := strings.Join(parts[:len(parts)-1], "/")
		if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
			host = "http://" + host
		}
		serverUrl = host
	}

	serverUrl = sanitizeServerUrl(serverUrl)

	// LAN Peer discovery check
	if *lanFlag {
		fmt.Printf("Scanning LAN for mDNS service with share code %s...\n", shareId)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		addr, filename, size, fingerprint, fileSHA256, passwordRequired, mdnsErr := lan.DiscoverService(ctx, shareId)
		cancel()

		if mdnsErr == nil {
			fmt.Printf("Peer found on LAN at %s!\n", addr)
			fmt.Printf("File: %s (%s)\n", filename, formatBytes(size))
			fmt.Printf("Fingerprint: %s\n", fingerprint)

			pwdToUse := *passwordFlag
			if passwordRequired && pwdToUse == "" {
				fmt.Print("This LAN share is password-protected. Enter password: ")
				pwdBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
				if err != nil {
					fmt.Printf("\nError reading password: %v\n", err)
					os.Exit(1)
				}
				fmt.Println()
				pwdToUse = strings.TrimSpace(string(pwdBytes))
			}

			sanitizedName := sanitizeFilename(filename)
			outputFilepath := sanitizedName
			var finalExtractDir string
			isArchive := strings.HasSuffix(filename, ".tar.gz")

			if isArchive {
				originalDirName := sanitizedName[:len(sanitizedName)-len(".tar.gz")]
				outputFilepath = originalDirName
				finalExtractDir = originalDirName
			}

			if destPath != "" {
				destFileInfo, statErr := os.Stat(destPath)
				if statErr == nil && destFileInfo.IsDir() {
					if isArchive {
						originalDirName := sanitizedName[:len(sanitizedName)-len(".tar.gz")]
						finalExtractDir = filepath.Join(destPath, originalDirName)
						outputFilepath = finalExtractDir
					} else {
						outputFilepath = filepath.Join(destPath, sanitizedName)
					}
				} else {
					parentDir := filepath.Dir(destPath)
					if _, pErr := os.Stat(parentDir); os.IsNotExist(pErr) {
						if *mkdirFlag || *mkdirShortFlag {
							err = os.MkdirAll(parentDir, 0755)
							if err != nil {
								fmt.Printf("Error creating directory %s: %v\n", parentDir, err)
								os.Exit(1)
							}
						} else {
							fmt.Printf("Error: Destination folder %s does not exist. Use --mkdir or -p flag to create it.\n", parentDir)
							os.Exit(1)
						}
					}
					outputFilepath = destPath
					finalExtractDir = destPath
				}
			}

			tempTarFile := outputFilepath
			if isArchive {
				tempTarFile = outputFilepath + ".download.tar.gz"
			}
			if keyHex != "" {
				tempTarFile = tempTarFile + ".enc"
			}

			fmt.Printf("Downloading directly from local peer over HTTPS...\n")
			printer := &ProgressPrinter{
				title:      "Downloading (LAN)...",
				total:      size,
				startTime:  time.Now(),
				firstPrint: true,
			}

			lanUrl := fmt.Sprintf("https://%s", addr)
			err = lan.DownloadFileLAN(lanUrl, tempTarFile, 0, fingerprint, shareId, pwdToUse, fileSHA256, func(written int64) {
				printer.Print(written)
			})

			if err == nil {
				// Decrypt if E2EE
				if keyHex != "" {
					decryptedFile := strings.TrimSuffix(tempTarFile, ".enc")
					fmt.Println("\nDecrypting file client-side (E2EE)...")
					err = DecryptFileStream(tempTarFile, decryptedFile, keyHex)
					os.Remove(tempTarFile)
					if err != nil {
						fmt.Printf("✗ Decryption error: %v\n", err)
						os.Exit(1)
					}
					tempTarFile = decryptedFile
				}

				if isArchive {
					tarReader, err := os.Open(tempTarFile)
					if err != nil {
						fmt.Printf("✗ Error: Opening download archive failed: %v\n", err)
						os.Remove(tempTarFile)
						os.Exit(1)
					}
					err = tarball.Unpack(tarReader, finalExtractDir)
					tarReader.Close()
					os.Remove(tempTarFile)
					if err != nil {
						fmt.Printf("✗ Error: Extraction failed: %v\n", err)
						os.Exit(1)
					}
					fmt.Printf("\n✓ LAN Download completed\n\nFile:\n%s\n\nDestination:\n%s\n\nSize:\n%s\n", filename, finalExtractDir, formatBytes(size))
				} else {
					fmt.Printf("\n✓ LAN Download completed\n\nFile:\n%s\n\nDestination:\n%s\n\nSize:\n%s\n", filename, outputFilepath, formatBytes(size))
				}
				os.Exit(0)
			}
			fmt.Printf("LAN download failed: %v. Falling back to cloud...\n", err)
		} else {
			fmt.Printf("No LAN peer found (%v). Falling back to cloud...\n", mdnsErr)
		}
	}

	// Fetch Share Metadata
	metaUrl := fmt.Sprintf("%s/api/v1/share/%s", serverUrl, shareId)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(metaUrl)
	if err != nil {
		fmt.Printf("✗ Error: Could not connect to server: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		fmt.Println("✗ Error: Share not found or has expired.")
		os.Exit(1)
	}
	if resp.StatusCode == 410 {
		fmt.Println("✗ Error: Share link has expired or reached its download limit.")
		os.Exit(1)
	}
	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		fmt.Printf("✗ Error: Server returned status %d: %s\n", resp.StatusCode, string(bodyBytes))
		os.Exit(1)
	}

	var meta ShareMeta
	err = json.NewDecoder(resp.Body).Decode(&meta)
	if err != nil {
		fmt.Printf("✗ Error: Failed to parse share metadata: %v\n", err)
		os.Exit(1)
	}

	password := *passwordFlag
	if meta.PasswordRequired && password == "" {
		fmt.Print("This share is password-protected. Enter password: ")
		pwdBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			fmt.Printf("\nError reading password: %v\n", err)
			os.Exit(1)
		}
		fmt.Println()
		password = strings.TrimSpace(string(pwdBytes))
	}

	// Authorize Download
	authUrl := fmt.Sprintf("%s/api/v1/share/%s/authorize-download", serverUrl, shareId)
	authReqBody, _ := json.Marshal(AuthorizeRequest{
		Password: password,
		Preview:  false,
	})

	authResp, err := client.Post(authUrl, "application/json", bytes.NewBuffer(authReqBody))
	if err != nil {
		fmt.Printf("✗ Error: Download authorization request failed: %v\n", err)
		os.Exit(1)
	}
	defer authResp.Body.Close()

	if authResp.StatusCode == 401 {
		fmt.Println("✗ Error: Incorrect password.")
		os.Exit(1)
	}
	if authResp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(authResp.Body)
		fmt.Printf("✗ Error: Download authorization failed (status %d): %s\n", authResp.StatusCode, string(bodyBytes))
		os.Exit(1)
	}

	var authData AuthorizeResponse
	err = json.NewDecoder(authResp.Body).Decode(&authData)
	if err != nil {
		fmt.Printf("✗ Error: Failed to parse authorization response: %v\n", err)
		os.Exit(1)
	}

	cleanFilename := cleanPrintName(meta.Filename)
	sanitizedName := sanitizeFilename(meta.Filename)
	outputFilepath := sanitizedName
	var finalExtractDir string

	isArchive := strings.HasSuffix(meta.Filename, ".tar.gz")
	if isArchive {
		originalDirName := sanitizedName[:len(sanitizedName)-len(".tar.gz")]
		outputFilepath = originalDirName
		finalExtractDir = originalDirName
	}

	if destPath != "" {
		destFileInfo, statErr := os.Stat(destPath)
		if statErr == nil && destFileInfo.IsDir() {
			if isArchive {
				originalDirName := sanitizedName[:len(sanitizedName)-len(".tar.gz")]
				finalExtractDir = filepath.Join(destPath, originalDirName)
				outputFilepath = finalExtractDir
			} else {
				outputFilepath = filepath.Join(destPath, sanitizedName)
			}
		} else {
			parentDir := filepath.Dir(destPath)
			if _, pErr := os.Stat(parentDir); os.IsNotExist(pErr) {
				if *mkdirFlag || *mkdirShortFlag {
					err = os.MkdirAll(parentDir, 0755)
					if err != nil {
						fmt.Printf("Error creating directory %s: %v\n", parentDir, err)
						os.Exit(1)
					}
				} else {
					fmt.Printf("Error: Destination folder %s does not exist. Use --mkdir or -p flag to create it.\n", parentDir)
					os.Exit(1)
				}
			}
			outputFilepath = destPath
			finalExtractDir = destPath
		}
	}

	absOut, err := filepath.Abs(outputFilepath)
	if err != nil {
		fmt.Printf("Error resolving output path: %v\n", err)
		os.Exit(1)
	}

	// Overwrite check
	if _, err = os.Stat(absOut); err == nil {
		forceOverwrite := *forceFlag || *forceShortFlag
		renameFile := *renameFlag || *renameShortFlag

		if !forceOverwrite && !renameFile {
			fmt.Printf("Error: Target '%s' already exists. Use --force (-f) to overwrite or --rename (-r) to save with suffix.\n", absOut)
			os.Exit(1)
		}

		if renameFile && !forceOverwrite {
			dir := filepath.Dir(absOut)
			base := filepath.Base(absOut)
			suffix := 1
			for {
				var newName string
				if isArchive {
					newName = fmt.Sprintf("%s (%d)", base, suffix)
				} else {
					ext := filepath.Ext(base)
					baseWithoutExt := base[:len(base)-len(ext)]
					newName = fmt.Sprintf("%s (%d)%s", baseWithoutExt, suffix, ext)
				}
				outputFilepath = filepath.Join(dir, newName)
				absOut, _ = filepath.Abs(outputFilepath)
				if _, err = os.Stat(absOut); os.IsNotExist(err) {
					break
				}
				suffix++
			}
			if isArchive {
				finalExtractDir = absOut
			}
		}
	}

	tempTarFile := absOut
	if isArchive {
		tempTarFile = absOut + ".download.tar.gz"
	}
	if keyHex != "" {
		tempTarFile = tempTarFile + ".enc"
	}

	rangeSupported := false
	probeReq, err := http.NewRequest("HEAD", authData.DownloadUrl, nil)
	if err == nil {
		probeResp, err := client.Do(probeReq)
		if err == nil {
			defer probeResp.Body.Close()
			if probeResp.Header.Get("Accept-Ranges") == "bytes" {
				rangeSupported = true
			}
		}
	}

	printer := &ProgressPrinter{
		title:      "Downloading...",
		total:      meta.Size,
		startTime:  time.Now(),
		firstPrint: true,
	}

	if rangeSupported {
		err = DownloadResumable(authData.DownloadUrl, tempTarFile, meta.HashValue, func(written int64, resumeOffset int64) {
			if printer.resumeOffset == 0 && resumeOffset > 0 {
				printer.resumeOffset = resumeOffset
				printer.resumeTime = time.Now()
			}
			printer.Print(written)
		})
	} else {
		downloadResp, err := http.Get(authData.DownloadUrl)
		if err != nil {
			fmt.Printf("✗ Error: Downloading file failed: %v\n", err)
			os.Exit(1)
		}
		defer downloadResp.Body.Close()

		if downloadResp.StatusCode != 200 {
			bodyBytes, _ := io.ReadAll(downloadResp.Body)
			fmt.Printf("✗ Error: Download failed (status %d): %s\n", downloadResp.StatusCode, string(bodyBytes))
			os.Exit(1)
		}

		outFd, err := os.OpenFile(tempTarFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			fmt.Printf("✗ Error: Creating output file failed: %v\n", err)
			os.Exit(1)
		}

		progDownload := &ProgressReader{
			reader:  downloadResp.Body,
			printer: printer,
		}

		downloadHasher := sha256.New()
		multiWriter := io.MultiWriter(outFd, downloadHasher)
		_, err = io.Copy(multiWriter, progDownload)
		outFd.Close()

		if err == nil {
			computedHex := hex.EncodeToString(downloadHasher.Sum(nil))
			if computedHex != meta.HashValue {
				fmt.Println("\n✗ Error: File integrity check failed!")
				os.Remove(tempTarFile)
				os.Exit(1)
			}
		}
	}

	if err != nil {
		os.Remove(tempTarFile)
		fmt.Printf("\n✗ Error: Download interrupted: %v\n", err)
		os.Exit(1)
	}

	// Decrypt if encrypted
	if keyHex != "" {
		decryptedFile := strings.TrimSuffix(tempTarFile, ".enc")
		fmt.Println("\nDecrypting file client-side (E2EE)...")
		err = DecryptFileStream(tempTarFile, decryptedFile, keyHex)
		os.Remove(tempTarFile)
		if err != nil {
			fmt.Printf("✗ Decryption error: %v\n", err)
			os.Exit(1)
		}
		tempTarFile = decryptedFile
	}

	if isArchive {
		tarReader, err := os.Open(tempTarFile)
		if err != nil {
			fmt.Printf("✗ Error: Opening download archive failed: %v\n", err)
			os.Remove(tempTarFile)
			os.Exit(1)
		}
		err = tarball.Unpack(tarReader, finalExtractDir)
		tarReader.Close()
		os.Remove(tempTarFile)
		if err != nil {
			fmt.Printf("✗ Error: Extraction failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\n✓ Download completed\n\nFile:\n%s\n\nDestination:\n%s\n\nSize:\n%s\n", cleanFilename, finalExtractDir, formatBytes(meta.Size))
	} else {
		fmt.Printf("\n✓ Download completed\n\nFile:\n%s\n\nDestination:\n%s\n\nSize:\n%s\n", cleanFilename, absOut, formatBytes(meta.Size))
	}

	notifyTransferComplete(meta.Filename)
}

type ProgressPrinter struct {
	title        string
	total        int64
	startTime    time.Time
	lastPrint    time.Time
	firstPrint   bool
	isFinished   bool
	resumeOffset int64
	resumeTime   time.Time
}

func (pp *ProgressPrinter) Print(read int64) {
	now := time.Now()
	if !pp.isFinished && read < pp.total && now.Sub(pp.lastPrint) < 100*time.Millisecond {
		return
	}
	pp.lastPrint = now

	percent := float64(0)
	if pp.total > 0 {
		percent = float64(read) / float64(pp.total)
	}
	percentInt := int(percent * 100)

	elapsed := time.Since(pp.startTime).Seconds()
	speed := 0.0
	if pp.resumeOffset > 0 && !pp.resumeTime.IsZero() {
		elapsedResume := time.Since(pp.resumeTime).Seconds()
		if elapsedResume > 0 {
			speed = float64(read-pp.resumeOffset) / elapsedResume
		}
	} else if elapsed > 0 {
		speed = float64(read) / elapsed
	}

	etaStr := "Calculating..."
	if pp.resumeOffset > 0 && read <= pp.resumeOffset {
		etaStr = "Resuming..."
	} else if speed > 0 && pp.total > 0 {
		remainingBytes := pp.total - read
		etaSeconds := float64(remainingBytes) / speed
		if etaSeconds <= 0 {
			etaStr = "0 seconds"
		} else if etaSeconds < 60 {
			etaStr = fmt.Sprintf("%d seconds", int(etaSeconds))
		} else if etaSeconds < 3600 {
			etaStr = fmt.Sprintf("%d minutes", int(etaSeconds/60))
		} else {
			etaStr = fmt.Sprintf("%d hours", int(etaSeconds/3600))
		}
	}
	if read >= pp.total {
		etaStr = "0 seconds"
		pp.isFinished = true
	}

	barWidth := 20
	completed := int(percent * float64(barWidth))
	if completed > barWidth {
		completed = barWidth
	}
	barStr := strings.Repeat("█", completed) + strings.Repeat("░", barWidth-completed)

	speedStr := fmt.Sprintf("%s/s", formatBytes(int64(speed)))
	transferredStr := fmt.Sprintf("%s / %s", formatBytes(read), formatBytes(pp.total))

	fmt.Printf("\r\033[K%s [%s] %d%% (%s) | %s | ETA: %s", pp.title, barStr, percentInt, transferredStr, speedStr, etaStr)
	if read >= pp.total {
		fmt.Println()
	}
}

type ProgressReader struct {
	reader  io.Reader
	printer *ProgressPrinter
	base    int64 // cumulative bytes already reported before THIS reader (multipart parts start mid-file)
	read    int64
}

func (pr *ProgressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	if n > 0 {
		pr.read += int64(n)
		pr.printer.Print(pr.base + pr.read)
	}
	return n, err
}

func getDirSize(path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size, err
}
