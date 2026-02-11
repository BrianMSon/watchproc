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
	pattern   = flag.String("p", "", "프로세스 이름 패턴 (부분 일치)")
	exact     = flag.Bool("e", false, "정확한 이름 일치")
	sortBy    = flag.String("s", "cpu", "정렬 기준: cpu, mem, pid, name")
	descOrder = flag.Bool("d", true, "내림차순 정렬")
	topN      = flag.Int("top", 0, "상위 N개만 표시 (0=전체)")
	noHeader  = flag.Bool("no-header", false, "헤더 숨기기")

	lastTermWidth  int
	lastTermHeight int
)

var startTime time.Time

var (
	paused    int32
	lastProcs []ProcessInfo
	displayMu sync.Mutex
	exitOnce  sync.Once
)

// colWidths 터미널 폭에 따른 동적 컬럼 너비
type colWidths struct {
	name    int
	created int
	user    int
	total   int
}

func calcColWidths(termCols int) colWidths {
	// 고정: PID(8)+sp(1)+CPU%(8)+sp(1)+MEM%(8)+sp(2)+MEM(10)+sp(1)+UPTIME(8)+sp(1)+STATUS(8)+sp(1) = 57
	// 가변 사이 sp: NAME 뒤(1) + CREATED 뒤(1) = 2 → 총 고정 59
	const fixedWidth = 59
	const minName = 12
	const minCreated = 10
	const minUser = 8

	if termCols < fixedWidth+minName+minCreated+minUser {
		termCols = fixedWidth + minName + minCreated + minUser
	}

	remaining := termCols - fixedWidth
	nameW := remaining * 45 / 100
	if nameW > 30 {
		nameW = 30
	}
	if nameW < minName {
		nameW = minName
	}
	createdW := remaining * 30 / 100
	if createdW > 20 {
		createdW = 20
	}
	if createdW < minCreated {
		createdW = minCreated
	}
	userW := remaining - nameW - createdW
	if userW < minUser {
		userW = minUser
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
func getProcesses(pattern string, exactMatch bool) ([]ProcessInfo, error) {
	procs, err := process.Processes()
	if err != nil {
		return nil, err
	}

	var result []ProcessInfo
	patternLower := strings.ToLower(pattern)

	for _, p := range procs {
		name, err := p.Name()
		if err != nil {
			continue
		}

		// 이름 필터링
		if pattern != "" {
			nameLower := strings.ToLower(name)
			if exactMatch {
				if nameLower != patternLower {
					continue
				}
			} else {
				if !strings.Contains(nameLower, patternLower) {
					continue
				}
			}
		}

		info := ProcessInfo{
			PID:  p.Pid,
			Name: name,
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

	// 폭에 맞춰 단계별로 정보 축소
	full := fmt.Sprintf("WatchProc [%s] | Pattern: %s | Interval: %.1fs | Found: %d processes",
		elapsed, patDisplay, interval, count)
	if len(full) <= w {
		fmt.Printf("\033[1;35mWatchProc\033[0m [\033[1;33m%s\033[0m] | Pattern: \033[1;33m%s\033[0m | Interval: %.1fs | Found: \033[1;32m%d\033[0m processes\n",
			elapsed, patDisplay, interval, count)
	} else if len(fmt.Sprintf("WatchProc [%s] | Pattern: %s | Interval: %.1fs | Found: %d",
		elapsed, patDisplay, interval, count)) <= w {
		// "processes" 제거
		fmt.Printf("\033[1;35mWatchProc\033[0m [\033[1;33m%s\033[0m] | Pattern: \033[1;33m%s\033[0m | Interval: %.1fs | Found: \033[1;32m%d\033[0m\n",
			elapsed, patDisplay, interval, count)
	} else if len(fmt.Sprintf("WatchProc [%s] | Pattern: %s | Found: %d",
		elapsed, patDisplay, count)) <= w {
		// Interval 제거
		fmt.Printf("\033[1;35mWatchProc\033[0m [\033[1;33m%s\033[0m] | Pattern: \033[1;33m%s\033[0m | Found: \033[1;32m%d\033[0m\n",
			elapsed, patDisplay, count)
	} else if len(fmt.Sprintf("WatchProc [%s] | P:%s | Found: %d",
		elapsedShort, patDisplay, count)) <= w {
		// 축약
		fmt.Printf("\033[1;35mWatchProc\033[0m [\033[1;33m%s\033[0m] | P:\033[1;33m%s\033[0m | Found: \033[1;32m%d\033[0m\n",
			elapsedShort, patDisplay, count)
	} else {
		// 최소
		fmt.Printf("\033[1;35mWatchProc\033[0m | \033[1;32m%d\033[0m\n", count)
	}

	fmt.Println(strings.Repeat("-", w))
}

func printTableHeader(cw colWidths) {
	fmt.Printf("\033[1;37m%-8s %-*s %8s %8s  %-10s %-*s %-8s %-*s %-8s\033[0m\n",
		"PID", cw.name, "NAME", "CPU%", "MEM%", "MEM", cw.created, "CREATED", "UPTIME", cw.user, "USER", "STATUS")
}

func printProcess(p ProcessInfo, cw colWidths) {
	name := p.Name

	// 사용자 이름 - 도메인\사용자 형식인 경우 사용자 부분만
	user := p.Username
	if idx := strings.LastIndex(user, "\\"); idx != -1 {
		user = user[idx+1:]
	}

	created := formatCreateTime(p.CreateTime)

	// CPU 색상 (높을수록 빨간색)
	cpuColor := "\033[0m"
	if p.CPUPercent > 50 {
		cpuColor = "\033[1;31m" // 빨간색
	} else if p.CPUPercent > 20 {
		cpuColor = "\033[1;33m" // 노란색
	} else if p.CPUPercent > 5 {
		cpuColor = "\033[1;32m" // 초록색
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

	fmt.Printf("%-8d %-*s %s%7.1f%%\033[0m %s%7.2f%%\033[0m  %-10s %-*s %-8s %-*s %-8s\n",
		p.PID,
		cw.name, name,
		cpuColor, p.CPUPercent,
		memColor, p.MemPercent,
		formatBytes(p.MemRSS),
		cw.created, created,
		formatDuration(p.CreateTime),
		cw.user, user,
		p.Status,
	)
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

	if !*noHeader {
		printHeader(pattern, *interval, len(procs), cw)
		printTableHeader(cw)
	}

	for _, p := range procs {
		printProcess(p, cw)
		clearLine()
	}

	// 이전 프레임보다 프로세스 수가 줄었을 때 남은 행 제거
	clearToEnd()

	// Footer
	fmt.Println()
	fmt.Printf("  \033[90mp:pause  q:quit\033[0m")
	clearLine()
}

func printPlainSnapshot(procs []ProcessInfo) {
	cols, _ := getTerminalSize()
	cw := calcColWidths(cols)
	if !*noHeader {
		printHeader(*pattern, *interval, len(procs), cw)
		printTableHeader(cw)
	}
	for _, p := range procs {
		printProcess(p, cw)
	}
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
		fmt.Fprintf(os.Stderr, "  watchproc -p chrome           # 'chrome' 포함 프로세스\n")
		fmt.Fprintf(os.Stderr, "  watchproc -p nginx -e         # 정확히 'nginx'인 프로세스\n")
		fmt.Fprintf(os.Stderr, "  watchproc -p java -s mem      # 메모리 기준 정렬\n")
		fmt.Fprintf(os.Stderr, "  watchproc -p python -top 10   # 상위 10개만 표시\n")
		fmt.Fprintf(os.Stderr, "  watchproc -n 1                # 1초마다 갱신\n")
		fmt.Fprintf(os.Stderr, "\nSort options: cpu, mem, pid, name, time\n")
		fmt.Fprintf(os.Stderr, "\nKeys (while running):\n")
		fmt.Fprintf(os.Stderr, "  p, Space    일시정지/재개\n")
		fmt.Fprintf(os.Stderr, "  q           종료 (마지막 스냅샷 유지)\n")
	}

	flag.Parse()
	startTime = time.Now()

	// 위치 인자로 패턴 지정 가능
	if flag.NArg() > 0 && *pattern == "" {
		*pattern = flag.Arg(0)
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
			case 'p', 'P', ' ':
				if atomic.CompareAndSwapInt32(&paused, 0, 1) {
					// 일시정지: 원래 화면으로 복귀, 전체 스냅샷을 스크롤 가능한 일반 출력으로 인쇄
					displayMu.Lock()
					snapshot := lastProcs
					displayMu.Unlock()
					fmt.Print("\033[?1049l")
					printPlainSnapshot(snapshot)
					fmt.Printf("\n  \033[1;43;30m PAUSED \033[0m  \033[90mp:resume  q:quit\033[0m\n")
				} else {
					// 재개: 대체 화면으로 복귀, 라이브 갱신 재시작
					atomic.StoreInt32(&paused, 0)
					fmt.Print("\033[?1049h")
					display(*pattern, *exact, true)
				}
			case 'q', 'Q', 3: // q 또는 Ctrl+C
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
