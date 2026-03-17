package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v3/process"
)

var (
	interval  = flag.Float64("n", 5.0, "갱신 간격 (초)")
	pattern   = flag.String("p", "", "프로세스 이름 패턴 (부분 일치, 쉼표나 공백으로 여러개 지정 가능)")
	exact     = flag.Bool("e", false, "정확한 이름 일치")
	sortBy    = flag.String("s", "cpu", "정렬 기준: cpu, mem, pid, name")
	descOrder = flag.Bool("d", true, "내림차순 정렬")
	topN      = flag.Int("top", 0, "상위 N개만 표시 (0=전체)")
	showPPID  = flag.Bool("ppid", false, "PPID (부모 프로세스 ID) 표시")
	noHeader  = flag.Bool("no-header", false, "헤더 숨기기")

	lastTermWidth  int
	lastTermHeight int
)

var startTime time.Time

var (
	paused      int32
	lastProcs   []ProcessInfo
	displayMu   sync.Mutex
	exitOnce    sync.Once
	selectedIdx int // 선택된 프로세스 행 인덱스 (-1 = 선택 없음)
	killMsg     string
	killMsgTime time.Time
)

// colWidths 터미널 폭에 따른 동적 컬럼 너비
type colWidths struct {
	name    int
	created int
	user    int
	total   int
}

func calcColWidths(termCols int) colWidths {
	// 고정 컬럼 최소폭: PID(5) CPU%(8) MEM%(8) MEM(6) UPTIME(5) STATUS(6) = 38
	// PPID 추가 시 +5
	// 가변 컬럼: NAME, CREATED, USER
	// 컬럼 수: 9 (PPID 포함 시 10), 구분 공백 = 컬럼수-1
	numCols := 9
	fixedMin := 38
	if *showPPID {
		numCols = 10
		fixedMin += 5
	}
	numGaps := numCols - 1

	// 가변 컬럼에 배분할 공간
	remaining := termCols - fixedMin
	if remaining < 12 {
		remaining = 12
	}

	// 구분 공백을 균등 배분
	gapTotal := numGaps
	if remaining > gapTotal+12 {
		// 여유 있으면 gap을 넓힘
		gapTotal = remaining * 20 / 100
		if gapTotal < numGaps {
			gapTotal = numGaps
		}
	}

	varSpace := remaining - gapTotal
	if varSpace < 3 {
		varSpace = 3
	}

	// 가변 컬럼 균등 배분
	nameW := varSpace / 3
	createdW := varSpace / 3
	userW := varSpace - nameW - createdW

	if nameW < 4 {
		nameW = 4
	}
	if createdW < 4 {
		createdW = 4
	}
	if userW < 4 {
		userW = 4
	}

	return colWidths{name: nameW, created: createdW, user: userW, total: termCols}
}


func formatElapsed(d time.Duration) string {
	d = d.Truncate(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func formatElapsedShort(d time.Duration) string {
	d = d.Truncate(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	if m > 0 {
		s := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", int(d.Seconds())%60)
}

// ProcessInfo 프로세스 정보 구조체
type ProcessInfo struct {
	PID        int32
	PPID       int32
	Name       string
	CreateTime time.Time
	CPUPercent float64
	MemPercent float32
	MemRSS     uint64 // Resident Set Size (bytes)
	Status     string
	Username   string
	CmdLine    string
}

// getProcesses 프로세스 목록 조회
func getProcesses(patternStr string, exactMatch bool) ([]ProcessInfo, error) {
	procs, err := process.Processes()
	if err != nil {
		return nil, err
	}

	var patterns []string
	if patternStr != "" {
		// 쉼표나 공백으로 구분된 패턴들을 분리
		f := func(c rune) bool {
			return c == ',' || c == ' '
		}
		for _, p := range strings.FieldsFunc(patternStr, f) {
			p = strings.TrimSpace(p)
			if p != "" {
				patterns = append(patterns, strings.ToLower(p))
			}
		}
	}

	var result []ProcessInfo

	for _, p := range procs {
		name, err := p.Name()
		if err != nil {
			continue
		}

		// 이름 필터링
		if len(patterns) > 0 {
			nameLower := strings.ToLower(name)
			match := false
			for _, pat := range patterns {
				if exactMatch {
					if nameLower == pat {
						match = true
						break
					}
				} else {
					if strings.Contains(nameLower, pat) {
						match = true
						break
					}
				}
			}
			if !match {
				continue
			}
		}

		info := ProcessInfo{
			PID:  p.Pid,
			Name: name,
		}

		// 부모 프로세스 ID
		if ppid, err := p.Ppid(); err == nil {
			info.PPID = ppid
		}

		// 생성 시간
		if createTime, err := p.CreateTime(); err == nil {
			info.CreateTime = time.UnixMilli(createTime)
		}

		// CPU 사용률
		if cpu, err := p.CPUPercent(); err == nil {
			info.CPUPercent = cpu
		}

		// 메모리 정보
		if memInfo, err := p.MemoryInfo(); err == nil && memInfo != nil {
			info.MemRSS = memInfo.RSS
		}
		if memPercent, err := p.MemoryPercent(); err == nil {
			info.MemPercent = memPercent
		}

		// 상태 (CPU 기반 복합 상태)
		if info.CPUPercent > 0.1 {
			info.Status = "active"
		} else {
			info.Status = "idle"
		}

		// 사용자
		if username, err := p.Username(); err == nil {
			info.Username = username
		}

		// 커맨드 라인 (너무 길면 자르기)
		if cmdline, err := p.Cmdline(); err == nil {
			info.CmdLine = cmdline
		}

		result = append(result, info)
	}

	return result, nil
}

// sortProcesses 프로세스 정렬
func sortProcesses(procs []ProcessInfo, sortBy string, desc bool) {
	sort.Slice(procs, func(i, j int) bool {
		var less bool
		switch sortBy {
		case "cpu":
			less = procs[i].CPUPercent < procs[j].CPUPercent
		case "mem":
			less = procs[i].MemPercent < procs[j].MemPercent
		case "pid":
			less = procs[i].PID < procs[j].PID
		case "name":
			less = strings.ToLower(procs[i].Name) < strings.ToLower(procs[j].Name)
		case "time":
			less = procs[i].CreateTime.Before(procs[j].CreateTime)
		default:
			less = procs[i].CPUPercent < procs[j].CPUPercent
		}
		if desc {
			return !less
		}
		return less
	})
}

// formatBytes 바이트를 읽기 쉬운 형식으로 변환
func formatBytes(bytes uint64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.1fG", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.1fM", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.1fK", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}

// formatDuration 실행 시간을 읽기 쉬운 형식으로 변환
func formatDuration(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	days := int(d.Hours() / 24)
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60

	if days > 0 {
		return fmt.Sprintf("%dd%dh", days, hours)
	} else if hours > 0 {
		return fmt.Sprintf("%dh%dm", hours, minutes)
	} else if minutes > 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// formatCreateTime 생성 시간을 읽기 쉬운 형식으로 변환
func formatCreateTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	now := time.Now()
	// 같은 해면 월-일 시:분, 다른 해면 연-월-일
	if t.Year() == now.Year() {
		return t.Format("01-02 15:04")
	}
	return t.Format("06-01-02 15:04")
}

func printHeader(pattern string, interval float64, count int, cw colWidths) {
	elapsed := formatElapsed(time.Since(startTime))
	elapsedShort := formatElapsedShort(time.Since(startTime))
	patDisplay := pattern
	if patDisplay == "" {
		patDisplay = "*"
	}
	w := cw.total

	// 폭에 맞춰 단계별로 정보 축소 (넓은 순 → 좁은 순)
	type variant struct {
		plain string
		color string
	}
	variants := []variant{
		{fmt.Sprintf("WatchProc [%s] | Pattern: %s | Interval: %.1fs | Found: %d processes",
			elapsed, patDisplay, interval, count),
			fmt.Sprintf("\033[1;35mWatchProc\033[0m [\033[1;33m%s\033[0m] | Pattern: \033[1;33m%s\033[0m | Interval: %.1fs | Found: \033[1;32m%d\033[0m processes",
				elapsed, patDisplay, interval, count)},
		{fmt.Sprintf("WatchProc [%s] | Pattern: %s | Interval: %.1fs | Found: %d",
			elapsed, patDisplay, interval, count),
			fmt.Sprintf("\033[1;35mWatchProc\033[0m [\033[1;33m%s\033[0m] | Pattern: \033[1;33m%s\033[0m | Interval: %.1fs | Found: \033[1;32m%d\033[0m",
				elapsed, patDisplay, interval, count)},
		{fmt.Sprintf("WatchProc [%s] | Pattern: %s | Found: %d",
			elapsed, patDisplay, count),
			fmt.Sprintf("\033[1;35mWatchProc\033[0m [\033[1;33m%s\033[0m] | Pattern: \033[1;33m%s\033[0m | Found: \033[1;32m%d\033[0m",
				elapsed, patDisplay, count)},
		{fmt.Sprintf("WatchProc [%s] | Ptn: %s | Fnd: %d",
			elapsed, patDisplay, count),
			fmt.Sprintf("\033[1;35mWatchProc\033[0m [\033[1;33m%s\033[0m] | Ptn: \033[1;33m%s\033[0m | Fnd: \033[1;32m%d\033[0m",
				elapsed, patDisplay, count)},
		{fmt.Sprintf("WP [%s] | %s | %d",
			elapsedShort, patDisplay, count),
			fmt.Sprintf("\033[1;35mWP\033[0m [\033[1;33m%s\033[0m] | \033[1;33m%s\033[0m | \033[1;32m%d\033[0m",
				elapsedShort, patDisplay, count)},
		{fmt.Sprintf("WP [%s] %d",
			elapsedShort, count),
			fmt.Sprintf("\033[1;35mWP\033[0m [\033[1;33m%s\033[0m] \033[1;32m%d\033[0m",
				elapsedShort, count)},
	}

	for _, v := range variants {
		if len(v.plain) <= w {
			fmt.Print(v.color)
			fmt.Print("\n")
			break
		}
	}

	fmt.Print(strings.Repeat("-", w-1))
	fmt.Print("\n")
}

func printTableHeaderAligned(cw colWidths, colMaxes []int) {
	var names []string
	if *showPPID {
		names = []string{"PID", "PPID", "NAME", "CPU%", "MEM%", "MEM", "CREATED", "UPTIME", "STATUS"}
	} else {
		names = []string{"PID", "NAME", "CPU%", "MEM%", "MEM", "CREATED", "UPTIME", "STATUS"}
	}

	// 헤더 필드를 데이터 칼럼 폭에 맞춰 패딩
	paddedNames := make([]string, len(names))
	for i, n := range names {
		paddedNames[i] = padField(n, len(n), colMaxes[i])
	}

	maxW := cw.total - 1
	line := "\033[1;37m" + buildLine(paddedNames, colMaxes, maxW) + "\033[0m"
	fmt.Print(truncateLine(line, maxW))
	fmt.Print("\n")
}

func printTableHeader(cw colWidths) {
	var names []string
	var nameLens []int
	if *showPPID {
		names = []string{"PID", "PPID", "NAME", "CPU%", "MEM%", "MEM", "CREATED", "UPTIME", "STATUS"}
	} else {
		names = []string{"PID", "NAME", "CPU%", "MEM%", "MEM", "CREATED", "UPTIME", "STATUS"}
	}
	for _, n := range names {
		nameLens = append(nameLens, len(n))
	}

	maxW := cw.total - 1
	line := "\033[1;37m" + buildLine(names, nameLens, maxW) + "\033[0m"
	fmt.Print(truncateLine(line, maxW))
	fmt.Print("\n")
}

// buildLine 필드들을 균등 간격으로 배치한 행 생성
func buildLine(fields []string, fieldLens []int, maxW int) string {
	numGaps := len(fields) - 1
	if numGaps <= 0 {
		if len(fields) == 1 {
			return fields[0]
		}
		return ""
	}

	// 전체 콘텐츠 폭 합산
	contentTotal := 0
	for _, fl := range fieldLens {
		contentTotal += fl
	}

	// 남는 공간을 gap에 균등 배분
	totalGap := maxW - contentTotal
	if totalGap < numGaps {
		totalGap = numGaps // 최소 1칸씩
	}

	var sb strings.Builder
	for i, f := range fields {
		sb.WriteString(f)
		if i < numGaps {
			// i번째 gap: 균등 배분 (나머지는 앞쪽 gap에 +1)
			gap := totalGap / numGaps
			if i < totalGap%numGaps {
				gap++
			}
			if gap < 1 {
				gap = 1
			}
			for g := 0; g < gap; g++ {
				sb.WriteByte(' ')
			}
		}
	}
	return sb.String()
}

// truncateLine ANSI escape 시퀀스를 제외한 표시 문자 수로 행을 자름
func truncateLine(s string, maxLen int) string {
	visible := 0
	i := 0
	for i < len(s) {
		if s[i] == '\033' {
			// ANSI escape sequence - skip until 'm'
			j := i + 1
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				j++ // skip 'm'
			}
			i = j
			continue
		}
		if visible >= maxLen {
			return s[:i] + "\033[0m"
		}
		visible++
		i++
	}
	return s
}

// procFields 프로세스 한 행의 필드 데이터
type procFields struct {
	fields    []string
	fieldLens []int
}

// buildProcFields 프로세스의 필드 문자열과 표시 길이를 생성
func buildProcFields(p ProcessInfo) procFields {
	name := p.Name

	created := formatCreateTime(p.CreateTime)

	// CPU 색상
	cpuColor := "\033[0m"
	if p.CPUPercent > 50 {
		cpuColor = "\033[1;31m"
	} else if p.CPUPercent > 20 {
		cpuColor = "\033[1;33m"
	} else if p.CPUPercent > 5 {
		cpuColor = "\033[1;32m"
	}

	// 메모리 색상
	memColor := "\033[0m"
	if p.MemPercent > 10 {
		memColor = "\033[1;31m"
	} else if p.MemPercent > 5 {
		memColor = "\033[1;33m"
	} else if p.MemPercent > 1 {
		memColor = "\033[1;32m"
	}

	pidStr := fmt.Sprintf("%d", p.PID)
	ppidStr := fmt.Sprintf("%d", p.PPID)
	cpuVal := fmt.Sprintf("%.1f%%", p.CPUPercent)
	cpuStr := fmt.Sprintf("%s%s\033[0m", cpuColor, cpuVal)
	memVal := fmt.Sprintf("%.2f%%", p.MemPercent)
	memStr := fmt.Sprintf("%s%s\033[0m", memColor, memVal)
	memBytes := formatBytes(p.MemRSS)
	uptime := formatDuration(p.CreateTime)

	var fields []string
	var fieldLens []int
	if *showPPID {
		fields = []string{pidStr, ppidStr, name, cpuStr, memStr, memBytes, created, uptime, p.Status}
		fieldLens = []int{len(pidStr), len(ppidStr), len(name), len(cpuVal), len(memVal), len(memBytes), len(created), len(uptime), len(p.Status)}
	} else {
		fields = []string{pidStr, name, cpuStr, memStr, memBytes, created, uptime, p.Status}
		fieldLens = []int{len(pidStr), len(name), len(cpuVal), len(memVal), len(memBytes), len(created), len(uptime), len(p.Status)}
	}
	return procFields{fields: fields, fieldLens: fieldLens}
}

// calcMaxColWidths 모든 행에서 각 칼럼의 최대 표시 길이 산출 (헤더 포함)
func calcMaxColWidths(rows []procFields) []int {
	if len(rows) == 0 {
		return nil
	}
	// 헤더 이름도 최소 폭에 반영
	var headerNames []string
	if *showPPID {
		headerNames = []string{"PID", "PPID", "NAME", "CPU%", "MEM%", "MEM", "CREATED", "UPTIME", "STATUS"}
	} else {
		headerNames = []string{"PID", "NAME", "CPU%", "MEM%", "MEM", "CREATED", "UPTIME", "STATUS"}
	}
	maxes := make([]int, len(rows[0].fieldLens))
	for i, n := range headerNames {
		if i < len(maxes) && len(n) > maxes[i] {
			maxes[i] = len(n)
		}
	}
	for _, r := range rows {
		for i, l := range r.fieldLens {
			if l > maxes[i] {
				maxes[i] = l
			}
		}
	}
	return maxes
}

// padField 필드를 targetLen까지 공백으로 오른쪽 패딩 (ANSI 코드 고려)
func padField(field string, visibleLen, targetLen int) string {
	if visibleLen >= targetLen {
		return field
	}
	return field + strings.Repeat(" ", targetLen-visibleLen)
}

func printProcess(pf procFields, colMaxes []int, maxW int, selected bool) {
	// 각 필드를 칼럼 최대 폭에 맞춰 패딩
	paddedFields := make([]string, len(pf.fields))
	for i, f := range pf.fields {
		paddedFields[i] = padField(f, pf.fieldLens[i], colMaxes[i])
	}

	line := buildLine(paddedFields, colMaxes, maxW)
	if selected {
		// 선택된 행: 반전 표시
		fmt.Print("\033[7m")
		fmt.Print(truncateLine(line, maxW))
		fmt.Print("\033[0m")
	} else {
		fmt.Print(truncateLine(line, maxW))
	}
	fmt.Print("\n")
}

func display(pattern string, exactMatch bool, firstRun bool) {
	displayMu.Lock()
	defer displayMu.Unlock()

	procs, err := getProcesses(pattern, exactMatch)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	// 정렬
	sortProcesses(procs, *sortBy, *descOrder)

	// 상위 N개만
	if *topN > 0 && len(procs) > *topN {
		procs = procs[:*topN]
	}
	lastProcs = procs

	// 터미널 크기 변경 감지
	width, height := getTerminalSize()
	terminalResized := width != lastTermWidth || height != lastTermHeight
	if terminalResized {
		lastTermWidth = width
		lastTermHeight = height
	}
	cw := calcColWidths(width)

	// 화면 갱신 (첫 실행 또는 터미널 크기 변경 시 전체 화면 지우기)
	if firstRun || terminalResized {
		clearScreen()
	}
	moveCursor(1, 1)

	// 모든 행의 필드를 미리 계산하고 칼럼 최대 폭 산출
	allFields := make([]procFields, len(procs))
	for i, p := range procs {
		allFields[i] = buildProcFields(p)
	}
	colMaxes := calcMaxColWidths(allFields)

	if !*noHeader {
		printHeader(pattern, *interval, len(procs), cw)
		if colMaxes != nil {
			printTableHeaderAligned(cw, colMaxes)
		} else {
			printTableHeader(cw)
		}
	}

	maxW := cw.total - 1
	// selectedIdx 범위 보정
	if selectedIdx >= len(allFields) {
		selectedIdx = len(allFields) - 1
	}
	for i, pf := range allFields {
		printProcess(pf, colMaxes, maxW, i == selectedIdx)
		clearLine()
	}

	// 이전 프레임보다 프로세스 수가 줄었을 때 남은 행 제거
	clearToEnd()

	// Footer
	fmt.Println()
	// kill 메시지 표시 (3초간)
	if killMsg != "" && time.Since(killMsgTime) < 3*time.Second {
		fmt.Printf("  %s", killMsg)
	} else {
		killMsg = ""
		if selectedIdx >= 0 {
			fmt.Printf("  \033[90m↑↓:select  Enter:kill  Esc:cancel  p:pause  q:quit\033[0m")
		} else {
			fmt.Printf("  \033[90mk:kill  p:pause  q:quit\033[0m")
		}
	}
	clearLine()
}

func printPlainSnapshot(procs []ProcessInfo) {
	cols, _ := getTerminalSize()
	cw := calcColWidths(cols)

	allFields := make([]procFields, len(procs))
	for i, p := range procs {
		allFields[i] = buildProcFields(p)
	}
	colMaxes := calcMaxColWidths(allFields)

	if !*noHeader {
		printHeader(*pattern, *interval, len(procs), cw)
		if colMaxes != nil {
			printTableHeaderAligned(cw, colMaxes)
		} else {
			printTableHeader(cw)
		}
	}
	maxW := cw.total - 1
	for _, pf := range allFields {
		printProcess(pf, colMaxes, maxW, false)
	}
}

func killSelected() {
	displayMu.Lock()
	defer displayMu.Unlock()
	if selectedIdx < 0 || selectedIdx >= len(lastProcs) {
		return
	}
	p := lastProcs[selectedIdx]
	proc, err := os.FindProcess(int(p.PID))
	if err != nil {
		killMsg = fmt.Sprintf("\033[1;31mFailed to find PID %d: %v\033[0m", p.PID, err)
		killMsgTime = time.Now()
		return
	}
	err = proc.Kill()
	if err != nil {
		killMsg = fmt.Sprintf("\033[1;31mFailed to kill %s (PID %d): %v\033[0m", p.Name, p.PID, err)
	} else {
		killMsg = fmt.Sprintf("\033[1;33mKilled %s (PID %d)\033[0m", p.Name, p.PID)
	}
	killMsgTime = time.Now()
}

func handleExit() {
	exitOnce.Do(func() {
		disableRawInput()
		restoreConsole()
		fmt.Print("\033[0m")
		if atomic.LoadInt32(&paused) == 0 {
			displayMu.Lock()
			snapshot := lastProcs
			displayMu.Unlock()
			fmt.Print("\033[?1049l")
			printPlainSnapshot(snapshot)
		}
		fmt.Println("WatchProc terminated.")
		os.Exit(0)
	})
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "WatchProc - 프로세스 모니터링 도구\n\n")
		fmt.Fprintf(os.Stderr, "Usage: watchproc [options] [pattern]\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  watchproc                     # 모든 프로세스 표시\n")
		fmt.Fprintf(os.Stderr, "  watchproc claude codex        # 'claude' 또는 'codex' 포함\n")
		fmt.Fprintf(os.Stderr, "  watchproc -p \"chrome,firefox\" # 'chrome' 또는 'firefox' 포함\n")
		fmt.Fprintf(os.Stderr, "  watchproc -p nginx -e         # 정확히 'nginx'인 프로세스\n")
		fmt.Fprintf(os.Stderr, "  watchproc -p java -s mem      # 메모리 기준 정렬\n")
		fmt.Fprintf(os.Stderr, "  watchproc -p python -top 10   # 상위 10개만 표시\n")
		fmt.Fprintf(os.Stderr, "  watchproc -n 1                # 1초마다 갱신\n")
		fmt.Fprintf(os.Stderr, "\nSort options: cpu, mem, pid, name, time\n")
		fmt.Fprintf(os.Stderr, "\nKeys (while running):\n")
		fmt.Fprintf(os.Stderr, "  ↑/↓         프로세스 선택\n")
		fmt.Fprintf(os.Stderr, "  k           선택한 프로세스 종료 (kill)\n")
		fmt.Fprintf(os.Stderr, "  Esc         선택 해제\n")
		fmt.Fprintf(os.Stderr, "  p, Space    일시정지/재개\n")
		fmt.Fprintf(os.Stderr, "  q           종료 (마지막 스냅샷 유지)\n")
	}

	flag.Parse()
	startTime = time.Now()
	selectedIdx = -1

	// -p 옵션과 위치 인자를 모두 합쳐서 패턴으로 사용
	argsPattern := strings.Join(flag.Args(), " ")
	if argsPattern != "" {
		if *pattern != "" {
			*pattern += " " + argsPattern
		} else {
			*pattern = argsPattern
		}
	}

	if *interval <= 0 {
		fmt.Fprintln(os.Stderr, "Error: 간격은 0보다 커야 합니다")
		os.Exit(1)
	}

	// 콘솔 설정
	setupConsole()
	enableRawInput()
	fmt.Print("\033[?1049h") // 대체 화면 버퍼 진입 (라이브 갱신용)

	// Ctrl+C 처리
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		handleExit()
	}()

	// 키 입력 처리 (p:일시정지/재개, q:종료)
	go func() {
		for {
			key, ok := readKey()
			if !ok {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			switch key {
			case KeyUp:
				if selectedIdx >= 0 {
					displayMu.Lock()
					if selectedIdx > 0 {
						selectedIdx--
					}
					displayMu.Unlock()
					if atomic.LoadInt32(&paused) == 0 {
						display(*pattern, *exact, false)
					}
				}
			case KeyDown:
				if selectedIdx >= 0 {
					displayMu.Lock()
					if selectedIdx < len(lastProcs)-1 {
						selectedIdx++
					}
					displayMu.Unlock()
					if atomic.LoadInt32(&paused) == 0 {
						display(*pattern, *exact, false)
					}
				}
			case 27: // Esc
				selectedIdx = -1
				if atomic.LoadInt32(&paused) == 0 {
					display(*pattern, *exact, false)
				}
			case 'k', 'K':
				if selectedIdx < 0 {
					// 선택 모드 진입
					selectedIdx = 0
					if atomic.LoadInt32(&paused) == 0 {
						display(*pattern, *exact, false)
					}
				}
			case 13: // Enter
				if selectedIdx >= 0 {
					killSelected()
					selectedIdx = -1
					if atomic.LoadInt32(&paused) == 0 {
						display(*pattern, *exact, false)
					}
				}
			case 'p', 'P', ' ':
				if atomic.CompareAndSwapInt32(&paused, 0, 1) {
					displayMu.Lock()
					snapshot := lastProcs
					displayMu.Unlock()
					fmt.Print("\033[?1049l")
					printPlainSnapshot(snapshot)
					fmt.Printf("\n  \033[1;43;30m PAUSED \033[0m  \033[90mp:resume  q:quit\033[0m\n")
				} else {
					atomic.StoreInt32(&paused, 0)
					fmt.Print("\033[?1049h")
					display(*pattern, *exact, true)
				}
			case 'q', 'Q', 3:
				handleExit()
			}
		}
	}()

	// 첫 실행
	display(*pattern, *exact, true)

	// 주기적 갱신
	ticker := time.NewTicker(time.Duration(*interval * float64(time.Second)))
	defer ticker.Stop()

	for range ticker.C {
		if atomic.LoadInt32(&paused) == 0 {
			display(*pattern, *exact, false)
		}
	}
}
