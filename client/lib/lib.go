package lib

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	_ "embed" // for embedding config.sh

	"gorm.io/gorm"

	"github.com/araddon/dateparse"
	"github.com/fatih/color"
	"github.com/google/uuid"
	"github.com/rodaine/table"

	"github.com/ddworken/hishtory/client/data"
	"github.com/ddworken/hishtory/client/hctx"
	"github.com/ddworken/hishtory/shared"
)

//go:embed config.sh
var ConfigShContents string

//go:embed config.zsh
var ConfigZshContents string

//go:embed config.fish
var ConfigFishContents string

var Version string = "Unknown"
var GitCommit string = "Unknown"

// 256KB ought to be enough for any reasonable cmd
var maxSupportedLineLengthForImport = 256_000

func Setup(userSecret string, isOffline bool) error {
	if userSecret == "" {
		userSecret = uuid.Must(uuid.NewRandom()).String()
	}
	fmt.Println("Setting secret hishtory key to " + string(userSecret))

	// Create and set the config
	var config hctx.ClientConfig
	config.UserSecret = userSecret
	config.IsEnabled = true
	config.DeviceId = uuid.Must(uuid.NewRandom()).String()
	config.ControlRSearchEnabled = true
	config.IsOffline = isOffline
	err := hctx.SetConfig(config)
	if err != nil {
		return fmt.Errorf("failed to persist config to disk: %w", err)
	}

	// Drop all existing data
	db, err := hctx.OpenLocalSqliteDb()
	if err != nil {
		return err
	}
	db.Exec("DELETE FROM history_entries")

	// Bootstrap from remote date
	if config.IsOffline {
		return nil
	}
	_, err = ApiGet("/api/v1/register?user_id=" + data.UserId(userSecret) + "&device_id=" + config.DeviceId)
	if err != nil {
		return fmt.Errorf("failed to register device with backend: %w", err)
	}

	respBody, err := ApiGet("/api/v1/bootstrap?user_id=" + data.UserId(userSecret) + "&device_id=" + config.DeviceId)
	if err != nil {
		return fmt.Errorf("failed to bootstrap device from the backend: %w", err)
	}
	var retrievedEntries []*shared.EncHistoryEntry
	err = json.Unmarshal(respBody, &retrievedEntries)
	if err != nil {
		return fmt.Errorf("failed to load JSON response: %w", err)
	}
	for _, entry := range retrievedEntries {
		decEntry, err := data.DecryptHistoryEntry(userSecret, *entry)
		if err != nil {
			return fmt.Errorf("failed to decrypt history entry from server: %w", err)
		}
		AddToDbIfNew(db, decEntry)
	}

	return nil
}

func AddToDbIfNew(db *gorm.DB, entry data.HistoryEntry) {
	tx := db.Where("local_username = ?", entry.LocalUsername)
	tx = tx.Where("hostname = ?", entry.Hostname)
	tx = tx.Where("command = ?", entry.Command)
	tx = tx.Where("current_working_directory = ?", entry.CurrentWorkingDirectory)
	tx = tx.Where("home_directory = ?", entry.HomeDirectory)
	tx = tx.Where("exit_code = ?", entry.ExitCode)
	tx = tx.Where("start_time = ?", entry.StartTime)
	tx = tx.Where("end_time = ?", entry.EndTime)
	var results []data.HistoryEntry
	tx.Limit(1).Find(&results)
	if len(results) == 0 {
		db.Create(normalizeEntryTimezone(entry))
		// TODO: check the error here and bubble it up
	}
}

func getCustomColumnValue(ctx context.Context, header string, entry data.HistoryEntry) (string, error) {
	for _, c := range entry.CustomColumns {
		if strings.EqualFold(c.Name, header) {
			return c.Val, nil
		}
	}
	config := hctx.GetConf(ctx)
	for _, c := range config.CustomColumns {
		if strings.EqualFold(c.ColumnName, header) {
			return "", nil
		}
	}
	return "", fmt.Errorf("failed to find a column matching the column name %#v (is there a typo?)", header)
}

func BuildTableRow(ctx context.Context, columnNames []string, entry data.HistoryEntry) ([]string, error) {
	row := make([]string, 0)
	for _, header := range columnNames {
		switch header {
		case "Hostname":
			row = append(row, entry.Hostname)
		case "CWD":
			row = append(row, entry.CurrentWorkingDirectory)
		case "Timestamp":
			row = append(row, entry.StartTime.Local().Format(hctx.GetConf(ctx).TimestampFormat))
		case "Runtime":
			if entry.EndTime.UnixMilli() == 0 {
				// An EndTime of zero means this is a pre-saved entry that never finished
				row = append(row, "N/A")
			} else {
				row = append(row, entry.EndTime.Local().Sub(entry.StartTime.Local()).Round(time.Millisecond).String())
			}
		case "Exit Code":
			row = append(row, fmt.Sprintf("%d", entry.ExitCode))
		case "Command":
			row = append(row, entry.Command)
		case "User":
			row = append(row, entry.LocalUsername)
		default:
			customColumnValue, err := getCustomColumnValue(ctx, header, entry)
			if err != nil {
				return nil, err
			}
			row = append(row, customColumnValue)
		}
	}
	return row, nil
}

func stringArrayToAnyArray(arr []string) []any {
	ret := make([]any, 0)
	for _, item := range arr {
		ret = append(ret, item)
	}
	return ret
}

func DisplayResults(ctx context.Context, results []*data.HistoryEntry, numResults int) error {
	config := hctx.GetConf(ctx)
	headerFmt := color.New(color.FgGreen, color.Underline).SprintfFunc()

	columns := make([]any, 0)
	for _, c := range config.DisplayedColumns {
		columns = append(columns, c)
	}
	tbl := table.New(columns...)
	tbl.WithHeaderFormatter(headerFmt)

	lastCommand := ""
	numRows := 0
	for _, entry := range results {
		if entry != nil && strings.TrimSpace(entry.Command) == strings.TrimSpace(lastCommand) && config.FilterDuplicateCommands {
			continue
		}
		row, err := BuildTableRow(ctx, config.DisplayedColumns, *entry)
		if err != nil {
			return err
		}
		tbl.AddRow(stringArrayToAnyArray(row)...)
		numRows += 1
		lastCommand = entry.Command
		if numRows >= numResults {
			break
		}
	}

	tbl.Print()
	return nil
}

func IsEnabled(ctx context.Context) (bool, error) {
	return hctx.GetConf(ctx).IsEnabled, nil
}

func CheckFatalError(err error) {
	if err != nil {
		_, filename, line, _ := runtime.Caller(1)
		log.Fatalf("hishtory v0.%s fatal error at %s:%d: %v", Version, filename, line, err)
	}
}

func stripZshWeirdness(cmd string) string {
	// Zsh has this weird behavior where sometimes commands are saved in the hishtory file
	// with a weird prefix. I've never been able to figure out why this happens, but we
	// can at least strip it.
	firstCommandBugRegex := regexp.MustCompile(`: \d+:\d;(.*)`)
	matches := firstCommandBugRegex.FindStringSubmatch(cmd)
	if len(matches) == 2 {
		return matches[1]
	}
	return cmd
}

func isBashWeirdness(cmd string) bool {
	// Bash has this weird behavior where the it has entries like `#1664342754` in the
	// history file. We want to skip these.
	firstCommandBugRegex := regexp.MustCompile(`^#\d+\s+$`)
	return firstCommandBugRegex.MatchString(cmd)
}

func ImportHistory(ctx context.Context, shouldReadStdin, force bool) (int, error) {
	config := hctx.GetConf(ctx)
	if config.HaveCompletedInitialImport && !force {
		// Don't run an import if we already have run one. This avoids importing the same entry multiple times.
		return 0, nil
	}
	homedir := hctx.GetHome(ctx)
	bashHistPath := filepath.Join(homedir, ".bash_history")
	historyEntries, err := readFileToArray(bashHistPath)
	if err != nil {
		return 0, fmt.Errorf("failed to parse bash history: %w", err)
	}
	zshHistPath := filepath.Join(homedir, ".zsh_history")
	extraEntries, err := readFileToArray(zshHistPath)
	if err != nil {
		return 0, fmt.Errorf("failed to parse zsh history: %w", err)
	}
	historyEntries = append(historyEntries, extraEntries...)
	extraEntries, err = parseFishHistory(homedir)
	if err != nil {
		return 0, fmt.Errorf("failed to parse fish history: %w", err)
	}
	historyEntries = append(historyEntries, extraEntries...)
	if histfile := os.Getenv("HISTFILE"); histfile != "" && histfile != zshHistPath && histfile != bashHistPath {
		extraEntries, err := readFileToArray(histfile)
		if err != nil {
			return 0, fmt.Errorf("failed to parse histfile: %w", err)
		}
		historyEntries = append(historyEntries, extraEntries...)
	}
	if shouldReadStdin {
		extraEntries, err = readStdin()
		if err != nil {
			return 0, fmt.Errorf("failed to read stdin: %w", err)
		}
		historyEntries = append(historyEntries, extraEntries...)
	}
	db := hctx.GetDb(ctx)
	currentUser, err := user.Current()
	if err != nil {
		return 0, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return 0, err
	}
	for _, cmd := range historyEntries {
		cmd := stripZshWeirdness(cmd)
		if isBashWeirdness(cmd) || strings.HasPrefix(cmd, " ") {
			// Skip it
			continue
		}
		entry := data.HistoryEntry{
			LocalUsername:           currentUser.Name,
			Hostname:                hostname,
			Command:                 cmd,
			CurrentWorkingDirectory: "Unknown",
			HomeDirectory:           homedir,
			ExitCode:                0,
			StartTime:               time.Now().UTC(),
			EndTime:                 time.Now().UTC(),
			DeviceId:                config.DeviceId,
		}
		err = ReliableDbCreate(db, entry)
		if err != nil {
			return 0, fmt.Errorf("failed to insert imported history entry: %w", err)
		}
	}
	err = Reupload(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to upload hishtory import: %w", err)
	}
	config.HaveCompletedInitialImport = true
	err = hctx.SetConfig(config)
	if err != nil {
		return 0, fmt.Errorf("failed to mark initial import as completed, this may lead to duplicate history entries: %w", err)
	}
	// Trigger a checkpoint so that these bulk entries are added from the WAL to the main DB
	db.Exec("PRAGMA wal_checkpoint")
	return len(historyEntries), nil
}

func readStdin() ([]string, error) {
	ret := make([]string, 0)
	in := bufio.NewReader(os.Stdin)
	for {
		s, err := in.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				return nil, err
			}
			break
		}
		s = strings.TrimSpace(s)
		if s != "" {
			ret = append(ret, s)
		}
	}
	return ret, nil
}

func parseFishHistory(homedir string) ([]string, error) {
	lines, err := readFileToArray(filepath.Join(homedir, ".local/share/fish/fish_history"))
	if err != nil {
		return nil, err
	}
	ret := make([]string, 0)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- cmd: ") {
			ret = append(ret, strings.SplitN(line, ": ", 2)[1])
		}
	}
	return ret, nil
}

func readFileToArray(path string) ([]string, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	buf := make([]byte, maxSupportedLineLengthForImport)
	scanner.Buffer(buf, maxSupportedLineLengthForImport)
	lines := make([]string, 0)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return lines, nil
}

func getServerHostname() string {
	if server := os.Getenv("HISHTORY_SERVER"); server != "" {
		return server
	}
	return "https://api.hishtory.dev"
}

func GetDownloadData() (shared.UpdateInfo, error) {
	respBody, err := ApiGet("/api/v1/download")
	if err != nil {
		return shared.UpdateInfo{}, fmt.Errorf("failed to download update info: %w", err)
	}
	var downloadData shared.UpdateInfo
	err = json.Unmarshal(respBody, &downloadData)
	if err != nil {
		return shared.UpdateInfo{}, fmt.Errorf("failed to parse update info: %w", err)
	}
	return downloadData, nil
}

func httpClient() *http.Client {
	return &http.Client{}
}

func ApiGet(path string) ([]byte, error) {
	if os.Getenv("HISHTORY_SIMULATE_NETWORK_ERROR") != "" {
		return nil, fmt.Errorf("simulated network error: dial tcp: lookup api.hishtory.dev")
	}
	start := time.Now()
	req, err := http.NewRequest("GET", getServerHostname()+path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create GET: %w", err)
	}
	req.Header.Set("X-Hishtory-Version", "v0."+Version)
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to GET %s%s: %w", getServerHostname(), path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("failed to GET %s%s: status_code=%d", getServerHostname(), path, resp.StatusCode)
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body from GET %s%s: %w", getServerHostname(), path, err)
	}
	duration := time.Since(start)
	hctx.GetLogger().Infof("ApiGet(%#v): %s\n", path, duration.String())
	return respBody, nil
}

func ApiPost(path, contentType string, data []byte) ([]byte, error) {
	if os.Getenv("HISHTORY_SIMULATE_NETWORK_ERROR") != "" {
		return nil, fmt.Errorf("simulated network error: dial tcp: lookup api.hishtory.dev")
	}
	start := time.Now()
	req, err := http.NewRequest("POST", getServerHostname()+path, bytes.NewBuffer(data))
	if err != nil {
		return nil, fmt.Errorf("failed to create POST: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Hishtory-Version", "v0."+Version)
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to POST %s: %w", getServerHostname()+path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("failed to POST %s: status_code=%d", getServerHostname()+path, resp.StatusCode)
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body from POST %s: %w", getServerHostname()+path, err)
	}
	duration := time.Since(start)
	hctx.GetLogger().Infof("ApiPost(%#v): %s\n", getServerHostname()+path, duration.String())
	return respBody, nil
}

func IsOfflineError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "dial tcp: lookup api.hishtory.dev") ||
		strings.Contains(err.Error(), "connect: network is unreachable") ||
		strings.Contains(err.Error(), "read: connection reset by peer") ||
		strings.Contains(err.Error(), ": EOF") ||
		strings.Contains(err.Error(), ": status_code=502") ||
		strings.Contains(err.Error(), ": status_code=503") ||
		strings.Contains(err.Error(), ": i/o timeout") ||
		strings.Contains(err.Error(), "connect: operation timed out") ||
		strings.Contains(err.Error(), "net/http: TLS handshake timeout") ||
		strings.Contains(err.Error(), "connect: connection refused")
}

func normalizeEntryTimezone(entry data.HistoryEntry) data.HistoryEntry {
	entry.StartTime = entry.StartTime.UTC()
	entry.EndTime = entry.EndTime.UTC()
	return entry
}

func RetryingDbFunction(dbFunc func() error) error {
	var err error = nil
	i := 0
	for i = 0; i < 10; i++ {
		err = dbFunc()
		if err == nil {
			return nil
		}
		errMsg := err.Error()
		if strings.Contains(errMsg, "database is locked (5) (SQLITE_BUSY)") || strings.Contains(errMsg, "database is locked (261)") {
			time.Sleep(time.Duration(i*rand.Intn(100)) * time.Millisecond)
			continue
		}
		if strings.Contains(errMsg, "UNIQUE constraint failed: history_entries.") {
			return nil
		}
		return fmt.Errorf("unrecoverable sqlite error: %w", err)
	}
	return fmt.Errorf("failed to execute DB transaction even with %d retries: %w", i, err)
}

func ReliableDbCreate(db *gorm.DB, entry data.HistoryEntry) error {
	entry = normalizeEntryTimezone(entry)
	return RetryingDbFunction(func() error {
		return db.Create(entry).Error
	})
}

func EncryptAndMarshal(config hctx.ClientConfig, entries []*data.HistoryEntry) ([]byte, error) {
	var encEntries []shared.EncHistoryEntry
	for _, entry := range entries {
		encEntry, err := data.EncryptHistoryEntry(config.UserSecret, *entry)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt history entry")
		}
		encEntry.DeviceId = config.DeviceId
		encEntries = append(encEntries, encEntry)
	}
	jsonValue, err := json.Marshal(encEntries)
	if err != nil {
		return jsonValue, fmt.Errorf("failed to marshal encrypted history entry: %w", err)
	}
	return jsonValue, nil
}

func Reupload(ctx context.Context) error {
	config := hctx.GetConf(ctx)
	if config.IsOffline {
		return nil
	}
	entries, err := Search(ctx, hctx.GetDb(ctx), "", 0)
	if err != nil {
		return fmt.Errorf("failed to reupload due to failed search: %w", err)
	}
	for _, chunk := range shared.Chunks(entries, 100) {
		jsonValue, err := EncryptAndMarshal(config, chunk)
		if err != nil {
			return fmt.Errorf("failed to reupload due to failed encryption: %w", err)
		}
		_, err = ApiPost("/api/v1/submit?source_device_id="+config.DeviceId, "application/json", jsonValue)
		if err != nil {
			return fmt.Errorf("failed to reupload due to failed POST: %w", err)
		}
	}
	return nil
}

func RetrieveAdditionalEntriesFromRemote(ctx context.Context) error {
	db := hctx.GetDb(ctx)
	config := hctx.GetConf(ctx)
	if config.IsOffline {
		return nil
	}
	respBody, err := ApiGet("/api/v1/query?device_id=" + config.DeviceId + "&user_id=" + data.UserId(config.UserSecret))
	if IsOfflineError(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var retrievedEntries []*shared.EncHistoryEntry
	err = json.Unmarshal(respBody, &retrievedEntries)
	if err != nil {
		return fmt.Errorf("failed to load JSON response: %w", err)
	}
	for _, entry := range retrievedEntries {
		decEntry, err := data.DecryptHistoryEntry(config.UserSecret, *entry)
		if err != nil {
			return fmt.Errorf("failed to decrypt history entry from server: %w", err)
		}
		AddToDbIfNew(db, decEntry)
	}
	return ProcessDeletionRequests(ctx)
}

func ProcessDeletionRequests(ctx context.Context) error {
	config := hctx.GetConf(ctx)
	if config.IsOffline {
		return nil
	}
	resp, err := ApiGet("/api/v1/get-deletion-requests?user_id=" + data.UserId(config.UserSecret) + "&device_id=" + config.DeviceId)
	if IsOfflineError(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var deletionRequests []*shared.DeletionRequest
	err = json.Unmarshal(resp, &deletionRequests)
	if err != nil {
		return err
	}
	db := hctx.GetDb(ctx)
	for _, request := range deletionRequests {
		for _, entry := range request.Messages.Ids {
			res := db.Where("device_id = ? AND end_time = ?", entry.DeviceId, entry.Date).Delete(&data.HistoryEntry{})
			if res.Error != nil {
				return fmt.Errorf("DB error: %w", res.Error)
			}
		}
	}
	return nil
}

func GetBanner(ctx context.Context) ([]byte, error) {
	config := hctx.GetConf(ctx)
	if config.IsOffline {
		return []byte{}, nil
	}
	url := "/api/v1/banner?commit_hash=" + GitCommit + "&user_id=" + data.UserId(config.UserSecret) + "&device_id=" + config.DeviceId + "&version=" + Version + "&forced_banner=" + os.Getenv("FORCED_BANNER")
	return ApiGet(url)
}

func parseTimeGenerously(input string) (time.Time, error) {
	input = strings.ReplaceAll(input, "_", " ")
	return dateparse.ParseLocal(input)
}

func MakeWhereQueryFromSearch(ctx context.Context, db *gorm.DB, query string) (*gorm.DB, error) {
	tokens, err := tokenize(query)
	if err != nil {
		return nil, fmt.Errorf("failed to tokenize query: %w", err)
	}
	tx := db.Model(&data.HistoryEntry{}).Where("true")
	for _, token := range tokens {
		if strings.HasPrefix(token, "-") {
			if token == "-" {
				// The entire token is a -, just ignore this token. Otherwise we end up
				// interpreting "-" as exluding literally all results which is pretty useless.
				continue
			}
			if containsUnescaped(token, ":") {
				query, v1, v2, err := parseAtomizedToken(ctx, token[1:])
				if err != nil {
					return nil, err
				}
				tx = tx.Where("NOT "+query, v1, v2)
			} else {
				query, v1, v2, v3, err := parseNonAtomizedToken(token[1:])
				if err != nil {
					return nil, err
				}
				tx = tx.Where("NOT "+query, v1, v2, v3)
			}
		} else if containsUnescaped(token, ":") {
			query, v1, v2, err := parseAtomizedToken(ctx, token)
			if err != nil {
				return nil, err
			}
			tx = tx.Where(query, v1, v2)
		} else {
			query, v1, v2, v3, err := parseNonAtomizedToken(token)
			if err != nil {
				return nil, err
			}
			tx = tx.Where(query, v1, v2, v3)
		}
	}
	return tx, nil
}

func Search(ctx context.Context, db *gorm.DB, query string, limit int) ([]*data.HistoryEntry, error) {
	if ctx == nil && query != "" {
		return nil, fmt.Errorf("lib.Search called with a nil context and a non-empty query (this should never happen)")
	}

	tx, err := MakeWhereQueryFromSearch(ctx, db, query)
	if err != nil {
		return nil, err
	}
	if hctx.GetConf(ctx).BetaMode {
		tx = tx.Order("start_time DESC")
	} else {
		tx = tx.Order("end_time DESC")
	}
	if limit > 0 {
		tx = tx.Limit(limit)
	}
	var historyEntries []*data.HistoryEntry
	result := tx.Find(&historyEntries)
	if result.Error != nil {
		return nil, fmt.Errorf("DB query error: %w", result.Error)
	}
	return historyEntries, nil
}

func parseNonAtomizedToken(token string) (string, interface{}, interface{}, interface{}, error) {
	wildcardedToken := "%" + unescape(token) + "%"
	return "(command LIKE ? OR hostname LIKE ? OR current_working_directory LIKE ?)", wildcardedToken, wildcardedToken, wildcardedToken, nil
}

func parseAtomizedToken(ctx context.Context, token string) (string, interface{}, interface{}, error) {
	splitToken := splitEscaped(token, ':', 2)
	field := unescape(splitToken[0])
	val := unescape(splitToken[1])
	switch field {
	case "user":
		return "(local_username = ?)", val, nil, nil
	case "host":
		fallthrough
	case "hostname":
		return "(instr(hostname, ?) > 0)", val, nil, nil
	case "cwd":
		return "(instr(current_working_directory, ?) > 0 OR instr(REPLACE(current_working_directory, '~/', home_directory), ?) > 0)", strings.TrimSuffix(val, "/"), strings.TrimSuffix(val, "/"), nil
	case "exit_code":
		return "(exit_code = ?)", val, nil, nil
	case "before":
		t, err := parseTimeGenerously(val)
		if err != nil {
			return "", nil, nil, fmt.Errorf("failed to parse before:%s as a timestamp: %w", val, err)
		}
		return "(CAST(strftime(\"%s\",start_time) AS INTEGER) < ?)", t.Unix(), nil, nil
	case "after":
		t, err := parseTimeGenerously(val)
		if err != nil {
			return "", nil, nil, fmt.Errorf("failed to parse after:%s as a timestamp: %w", val, err)
		}
		return "(CAST(strftime(\"%s\",start_time) AS INTEGER) > ?)", t.Unix(), nil, nil
	case "start_time":
		// Note that this atom probably isn't useful for interactive usage since it does exact matching, but we use it
		// internally for pre-saving history entries.
		t, err := parseTimeGenerously(val)
		if err != nil {
			return "", nil, nil, fmt.Errorf("failed to parse start_time:%s as a timestamp: %w", val, err)
		}
		return "(CAST(strftime(\"%s\",start_time) AS INTEGER) = ?)", t.Unix(), nil, nil
	case "end_time":
		// Note that this atom probably isn't useful for interactive usage since it does exact matching, but we use it
		// internally for pre-saving history entries.
		t, err := parseTimeGenerously(val)
		if err != nil {
			return "", nil, nil, fmt.Errorf("failed to parse end_time:%s as a timestamp: %w", val, err)
		}
		return "(CAST(strftime(\"%s\",end_time) AS INTEGER) = ?)", t.Unix(), nil, nil
	case "command":
		return "(instr(command, ?) > 0)", val, nil, nil
	default:
		knownCustomColumns := make([]string, 0)
		// Get custom columns that are defined on this machine
		conf := hctx.GetConf(ctx)
		for _, c := range conf.CustomColumns {
			knownCustomColumns = append(knownCustomColumns, c.ColumnName)
		}
		// Also get all ones that are in the DB
		names, err := getAllCustomColumnNames(ctx)
		if err != nil {
			return "", nil, nil, fmt.Errorf("failed to get custom column names from the DB: %w", err)
		}
		knownCustomColumns = append(knownCustomColumns, names...)
		// Check if the atom is for a custom column that exists and if it isn't, return an error
		isCustomColumn := false
		for _, ccName := range knownCustomColumns {
			if ccName == field {
				isCustomColumn = true
			}
		}
		if !isCustomColumn {
			return "", nil, nil, fmt.Errorf("search query contains unknown search atom '%s' that doesn't match any column names", field)
		}
		// Build the where clause for the custom column
		return "EXISTS (SELECT 1 FROM json_each(custom_columns) WHERE json_extract(value, '$.name') = ? and instr(json_extract(value, '$.value'), ?) > 0)", field, val, nil
	}
}

func getAllCustomColumnNames(ctx context.Context) ([]string, error) {
	db := hctx.GetDb(ctx)
	query := `
	SELECT DISTINCT json_extract(value, '$.name') as cc_name
	FROM history_entries 
	JOIN json_each(custom_columns)
	WHERE value IS NOT NULL
	LIMIT 10`
	rows, err := db.Raw(query).Rows()
	if err != nil {
		return nil, err
	}
	ccNames := make([]string, 0)
	for rows.Next() {
		var ccName string
		err = rows.Scan(&ccName)
		if err != nil {
			return nil, err
		}
		ccNames = append(ccNames, ccName)
	}
	return ccNames, nil
}

func tokenize(query string) ([]string, error) {
	if query == "" {
		return []string{}, nil
	}
	return splitEscaped(query, ' ', -1), nil
}

func splitEscaped(query string, separator rune, maxSplit int) []string {
	var token []rune
	var tokens []string
	splits := 1
	runeQuery := []rune(query)
	for i := 0; i < len(runeQuery); i++ {
		if (maxSplit < 0 || splits < maxSplit) && runeQuery[i] == separator {
			tokens = append(tokens, string(token))
			token = token[:0]
			splits++
		} else if runeQuery[i] == '\\' && i+1 < len(runeQuery) {
			token = append(token, runeQuery[i], runeQuery[i+1])
			i++
		} else {
			token = append(token, runeQuery[i])
		}
	}
	tokens = append(tokens, string(token))
	return tokens
}

func containsUnescaped(query string, token string) bool {
	runeQuery := []rune(query)
	for i := 0; i < len(runeQuery); i++ {
		if runeQuery[i] == '\\' && i+1 < len(runeQuery) {
			i++
		} else if string(runeQuery[i:i+len(token)]) == token {
			return true
		}
	}
	return false
}

func unescape(query string) string {
	runeQuery := []rune(query)
	var newQuery []rune
	for i := 0; i < len(runeQuery); i++ {
		if runeQuery[i] == '\\' {
			i++
		}
		if i < len(runeQuery) {
			newQuery = append(newQuery, runeQuery[i])
		}
	}
	return string(newQuery)
}

func GetDumpRequests(config hctx.ClientConfig) ([]*shared.DumpRequest, error) {
	if config.IsOffline {
		return make([]*shared.DumpRequest, 0), nil
	}
	resp, err := ApiGet("/api/v1/get-dump-requests?user_id=" + data.UserId(config.UserSecret) + "&device_id=" + config.DeviceId)
	if IsOfflineError(err) {
		return []*shared.DumpRequest{}, nil
	}
	if err != nil {
		return nil, err
	}
	var dumpRequests []*shared.DumpRequest
	err = json.Unmarshal(resp, &dumpRequests)
	return dumpRequests, err
}

func SendDeletionRequest(deletionRequest shared.DeletionRequest) error {
	data, err := json.Marshal(deletionRequest)
	if err != nil {
		return err
	}
	_, err = ApiPost("/api/v1/add-deletion-request", "application/json", data)
	if err != nil {
		return fmt.Errorf("failed to send deletion request to backend service, this may cause commands to not get deleted on other instances of hishtory: %w", err)
	}
	return nil
}
