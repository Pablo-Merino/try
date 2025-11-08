package main

import (
    "bufio"
    "errors"
    "fmt"
    "os"
    "os/exec"
    "path/filepath"
    "regexp"
    "sort"
    "strconv"
    "strings"
    "time"
    "unicode"
)

// ANSI token printer (mirrors Ruby/Python tokens)
func uiPrint(text string, w *os.File) {
    if w == nil {
        w = os.Stderr
    }

    var output string
    if isTTY(w) {
        // For TTY: expand tokens to ANSI codes
        replacer := strings.NewReplacer(
            "{text}", "\x1b[39m",
            "{dim_text}", "\x1b[90m",
            "{h1}", "\x1b[1;33m",
            "{h2}", "\x1b[1;36m",
            "{highlight}", "\x1b[1;33m",
            "{reset}", "\x1b[0m",
            "{reset_bg}", "\x1b[49m",
            "{reset_fg}", "\x1b[39m",
            "{clear_screen}", "\x1b[2J",
            "{clear_line}", "\x1b[2K",
            "{home}", "\x1b[H",
            "{hide_cursor}", "\x1b[?25l",
            "{show_cursor}", "\x1b[?25h",
            "{start_selected}", "\x1b[6m",
            "{end_selected}", "\x1b[0m",
        )
        output = replacer.Replace(text)
    } else {
        // For non-TTY: strip all tokens
        re := regexp.MustCompile(`\{[^}]*\}`)
        output = re.ReplaceAllString(text, "")
    }

    _, _ = w.WriteString(output)
}

func printGlobalHelp(defaultPath string) {
    w := os.Stdout
    uiPrint("{h1}try something!{reset}\n\n", w)
    uiPrint("Lightweight experiments for people with ADHD\n\n", w)
    uiPrint("this tool is not meant to be used directly,\n", w)
    uiPrint("but added to your ~/.zshrc or ~/.bashrc:\n\n", w)
    uiPrint("  {highlight}eval \"$(try init ~/src/tries)\"{reset}\n\n", w)
    uiPrint("for fish shell, add to ~/.config/fish/config.fish:\n\n", w)
    uiPrint("  {highlight}eval (try init ~/src/tries | string collect){reset}\n\n", w)
    uiPrint("{h2}Usage:{text}\n\n", w)
    uiPrint("  init [--path PATH]  # Initialize shell function for aliasing\n", w)
    uiPrint("  cd [QUERY] [name?]  # Interactive selector; Git URL shorthand supported\n", w)
    uiPrint("  clone <git-uri> [name]  # Clone git repo into date-prefixed directory\n", w)
    uiPrint("  worktree dir [name]  # Create date-prefixed dir; add worktree from CWD if git repo\n", w)
    uiPrint("  worktree <repo-path> [name]  # Same as above, but source repo is <repo-path>\n\n", w)
    uiPrint("{h2}Clone Examples:{text}\n\n", w)
    uiPrint("  try clone https://github.com/tobi/try.git\n", w)
    uiPrint("  # Creates: 2025-08-27-tobi-try\n\n", w)
    uiPrint("  try clone https://github.com/tobi/try.git my-fork\n", w)
    uiPrint("  # Creates: my-fork\n\n", w)
    uiPrint("  try https://github.com/tobi/try.git\n", w)
    uiPrint("  # Shorthand for clone (same as first example)\n\n", w)
    uiPrint("{h2}Worktree Examples:{text}\n\n", w)
    uiPrint("  try worktree dir\n", w)
    uiPrint("  # From current git repo, creates: 2025-08-27-repo-name and adds detached worktree\n\n", w)
    uiPrint("  try worktree ~/src/github.com/tobi/try my-branch\n", w)
    uiPrint("  # From given repo path, creates: 2025-08-27-my-branch and adds detached worktree\n\n", w)
    uiPrint("{h2}Defaults:{reset}\n", w)
    uiPrint("  Default path: {dim_text}~/src/tries{reset} (override with --path on commands)\n", w)
    uiPrint("  Current default: {dim_text}"+defaultPath+"{reset}\n", w)
}

func isTTY(f *os.File) bool {
    fi, err := f.Stat()
    if err != nil {
        return false
    }
    return (fi.Mode() & os.ModeCharDevice) != 0
}

type tryDir struct {
    Basename string
    Path     string
    Ctime    float64
    Mtime    float64
    Score    float64
}

type selector struct {
    basePath     string
    inputBuffer  string
    cursorPos    int
    scrollOffset int
    termW        int
    termH        int
    tries        []tryDir
    selected     *result
    deleteStatus string
}

type result struct {
    Type string // "cd" or "mkdir"
    Path string
}

type task struct {
    Type string // "target", "mkdir", "git-clone", "git-worktree", "touch", "cd", "echo"
    Path string
    URI  string // for git-clone
    Repo string // for git-worktree
    Msg  string // for echo
}

func newSelector(search, basePath string) *selector {
    s := &selector{
        basePath:     basePath,
        inputBuffer:  strings.ReplaceAll(strings.TrimSpace(strings.ReplaceAll(search, "\t", " ")), " ", "-"),
        cursorPos:    0,
        scrollOffset: 0,
        termW:        80,
        termH:        24,
    }
    if err := os.MkdirAll(basePath, 0o755); err != nil {
        _ = err
    }
    return s
}

func (s *selector) run() (*result, error) {
    if !isTTY(os.Stdin) || !isTTY(os.Stderr) {
        fmt.Fprintln(os.Stderr, "Error: try requires an interactive terminal")
        return nil, errors.New("no tty")
    }

    s.updateTermSize()
    uiPrint("{hide_cursor}{clear_screen}{home}", nil)

    fd := int(os.Stdin.Fd())
    st, err := enableRaw(fd)
    if err != nil {
        return nil, err
    }
    defer restoreTerm(fd, st)
    defer uiPrint("{clear_screen}{home}{show_cursor}", nil)

    for {
        s.render()
        key := s.readKey()
        totalItems := len(s.getTries()) + 1
        switch key {
        case "\x1b[A", "\x10", "\x0b": // Up or Ctrl-P or Ctrl-K
            if s.cursorPos > 0 {
                s.cursorPos--
            }
        case "\x1b[B", "\x0e", "\n": // Down or Ctrl-N or Ctrl-J
            if s.cursorPos < totalItems-1 {
                s.cursorPos++
            }
        case "\r":
            tries := s.getTries()
            if s.cursorPos < len(tries) {
                s.handleSelection(tries[s.cursorPos])
            } else {
                s.handleCreateNew(fd, st)
            }
            if s.selected != nil {
                return s.selected, nil
            }
        case "\x7f", "\b":
            if len(s.inputBuffer) > 0 {
                s.inputBuffer = s.inputBuffer[:len(s.inputBuffer)-1]
            }
            s.cursorPos = 0
        case "\x04": // Ctrl-D
            tries := s.getTries()
            if s.cursorPos < len(tries) {
                s.handleDelete(tries[s.cursorPos], fd, st)
            }
        case "\x03", "\x1b": // Ctrl-C or ESC
            return nil, nil
        default:
            if len(key) == 1 {
                r := []rune(key)[0]
                if (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("-_. ", r)) && r >= 32 {
                    s.inputBuffer += string(r)
                    s.cursorPos = 0
                }
            }
        }
    }
}

func (s *selector) updateTermSize() {
    w, h := getWinSize(int(os.Stdin.Fd()))
    if w <= 0 {
        w = 80
    }
    if h <= 0 {
        h = 24
    }
    s.termW, s.termH = w, h
}

func (s *selector) loadTries() []tryDir {
    if s.tries != nil {
        return s.tries
    }
    entries, err := os.ReadDir(s.basePath)
    if err != nil {
        return nil
    }
    var out []tryDir
    for _, e := range entries {
        name := e.Name()
        if name == "." || name == ".." {
            continue
        }
        p := filepath.Join(s.basePath, name)
        fi, err := os.Stat(p)
        if err != nil {
            continue
        }
        if !fi.IsDir() {
            continue
        }
        ctime, mtime := extractTimes(fi)
        out = append(out, tryDir{Basename: name, Path: p, Ctime: ctime, Mtime: mtime})
    }
    s.tries = out
    return out
}

func (s *selector) getTries() []tryDir {
    base := s.loadTries()
    list := make([]tryDir, 0, len(base))
    for _, t := range base {
        tt := t
        tt.Score = calculateScore(t.Basename, s.inputBuffer, t.Ctime, t.Mtime)
        list = append(list, tt)
    }
    if s.inputBuffer == "" {
        sort.Slice(list, func(i, j int) bool { return list[i].Score > list[j].Score })
        return list
    }
    filtered := make([]tryDir, 0, len(list))
    for _, t := range list {
        if t.Score > 0 {
            filtered = append(filtered, t)
        }
    }
    sort.Slice(filtered, func(i, j int) bool { return filtered[i].Score > filtered[j].Score })
    return filtered
}

func calculateScore(text, query string, ctime, mtime float64) float64 {
    score := 0.0
    if len(text) >= 11 && text[4] == '-' && text[7] == '-' && text[10] == '-' {
        if _, err := strconv.Atoi(text[0:4]); err == nil {
            score += 2.0
        }
    }
    if query != "" {
        tl := strings.ToLower(text)
        ql := strings.ToLower(query)
        qchars := []rune(ql)
        lastPos := -1
        qidx := 0
        for pos, r := range []rune(tl) {
            if qidx >= len(qchars) {
                break
            }
            if r != qchars[qidx] {
                continue
            }
            score += 1.0
            if pos == 0 || !isAlphaNumRune([]rune(tl)[pos-1]) {
                score += 1.0
            }
            if lastPos >= 0 {
                gap := pos - lastPos - 1
                score += 1.0 / sqrt(float64(gap+1))
            }
            lastPos = pos
            qidx++
        }
        if qidx < len(qchars) {
            return 0.0
        }
        if lastPos >= 0 {
            score *= float64(len(qchars)) / float64(lastPos+1)
        }
        score *= 10.0 / (float64(len(text)) + 10.0)
    }
    now := float64(time.Now().UnixNano()) / 1e9
    if ctime > 0 {
        days := (now - ctime) / 86400.0
        score += 2.0 / sqrt(days+1)
    }
    if mtime > 0 {
        hours := (now - mtime) / 3600.0
        score += 3.0 / sqrt(hours+1)
    }
    return score
}

func isAlphaNumRune(r rune) bool {
    return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func sqrt(x float64) float64 { // small inline sqrt via Newton's method to avoid math import
    if x <= 0 {
        return 0
    }
    z := x
    for i := 0; i < 10; i++ {
        z = 0.5 * (z + x/z)
    }
    return z
}

func (s *selector) readKey() string {
    buf := make([]byte, 1)
    n, _ := os.Stdin.Read(buf)
    if n == 0 {
        return ""
    }
    if buf[0] == 0x1b { // ESC
        // Try to read a few more bytes without blocking
        _ = setNonblock(int(os.Stdin.Fd()), true)
        defer setNonblock(int(os.Stdin.Fd()), false)
        seq := []byte{buf[0]}
        tmp := make([]byte, 4)
        for i := 0; i < 4; i++ {
            n2, err := os.Stdin.Read(tmp[i : i+1])
            if err != nil || n2 == 0 {
                break
            }
            seq = append(seq, tmp[i])
        }
        return string(seq)
    }
    return string(buf)
}

func (s *selector) render() {
    s.updateTermSize()
    uiPrint("{clear_screen}{home}", nil)
    sep := strings.Repeat("─", max(1, s.termW-1))
    uiPrint("{h1}📁 Try Directory Selection{text}\r\n", nil)
    uiPrint("{dim_text}"+sep+"{text}\r\n", nil)
    uiPrint("{highlight}Search: {text}"+s.inputBuffer+"\r\n", nil)
    uiPrint("{dim_text}"+sep+"{text}\r\n", nil)

    tries := s.getTries()
    total := len(tries) + 1
    maxVisible := max(3, s.termH-8)
    if s.cursorPos < s.scrollOffset {
        s.scrollOffset = s.cursorPos
    } else if s.cursorPos >= s.scrollOffset+maxVisible {
        s.scrollOffset = s.cursorPos - maxVisible + 1
    }
    end := min(s.scrollOffset+maxVisible, total)
    for idx := s.scrollOffset; idx < end; idx++ {
        if idx == len(tries) && len(tries) > 0 && idx >= s.scrollOffset {
            uiPrint("\r\n", nil)
        }
        isSel := idx == s.cursorPos
        if isSel {
            uiPrint("{highlight}→ {text}", nil)
        } else {
            uiPrint("  ", nil)
        }
        if idx < len(tries) {
            td := tries[idx]
            uiPrint("📁 ", nil)
            if isSel {
                uiPrint("{start_selected}", nil)
            }
            base := td.Basename
            display := base
            if m := splitDateName(base); m != nil {
                datePart := m[0]
                namePart := m[1]
                uiPrint("{dim_text}"+datePart+"{text}", nil)
                if s.inputBuffer != "" && strings.Contains(s.inputBuffer, "-") {
                    uiPrint("{highlight}-{text}", nil)
                } else {
                    uiPrint("{dim_text}-{text}", nil)
                }
                if s.inputBuffer != "" {
                    uiPrint(highlightMatches(namePart, s.inputBuffer), nil)
                } else {
                    uiPrint(namePart, nil)
                }
                display = datePart + "-" + namePart
            } else {
                if s.inputBuffer != "" {
                    uiPrint(highlightMatches(base, s.inputBuffer), nil)
                } else {
                    uiPrint(base, nil)
                }
            }
            timeText := formatRelativeTime(td.Mtime)
            scoreText := fmt.Sprintf("%.1f", td.Score)
            meta := timeText + ", " + scoreText
            metaWidth := len(meta) + 1
            textWidth := len(display)
            padding := max(1, s.termW-5-textWidth-metaWidth)
            uiPrint(strings.Repeat(" ", padding), nil)
            uiPrint(" {dim_text}"+meta+"{text}", nil)
        } else {
            uiPrint("+ ", nil)
            if isSel {
                uiPrint("{start_selected}", nil)
            }
            display := "Create new"
            if s.inputBuffer != "" {
                display = "Create new: " + s.inputBuffer
            }
            uiPrint(display, nil)
            padding := max(1, s.termW-5-len(display))
            uiPrint(strings.Repeat(" ", padding), nil)
        }
        uiPrint("{end_selected}{text}\r\n", nil)
    }
    if total > maxVisible {
        uiPrint("{dim_text}"+sep+"{text}\r\n", nil)
        uiPrint(fmt.Sprintf("{dim_text}[%d-%d/%d]{text}\r\n", s.scrollOffset+1, end, total), nil)
    }
    uiPrint("{dim_text}"+sep+"{text}\r\n", nil)
    if s.deleteStatus != "" {
        uiPrint("{highlight}"+s.deleteStatus+"{text}\r\n", nil)
        s.deleteStatus = "" // Clear after displaying
    } else {
        uiPrint("{dim_text}↑↓/Ctrl-P,N,J,K: Navigate  Enter: Select  Ctrl-D: Delete  ESC: Cancel{text}\r\n", nil)
    }
    os.Stderr.Sync()
}

func splitDateName(s string) []string {
    if len(s) >= 11 && s[4] == '-' && s[7] == '-' && s[10] == '-' {
        return []string{s[:10], s[11:]}
    }
    return nil
}

func formatRelativeTime(mtime float64) string {
    if mtime <= 0 {
        return "?"
    }
    secs := float64(time.Now().UnixNano())/1e9 - mtime
    mins := secs / 60
    hrs := mins / 60
    days := hrs / 24
    if secs < 10 {
        return "just now"
    } else if mins < 60 {
        return fmt.Sprintf("%dm ago", int(mins))
    } else if hrs < 24 {
        return fmt.Sprintf("%dh ago", int(hrs))
    } else if days < 30 {
        return fmt.Sprintf("%dd ago", int(days))
    } else if days < 365 {
        return fmt.Sprintf("%dmo ago", int(days/30))
    }
    return fmt.Sprintf("%dy ago", int(days/365))
}

func highlightMatches(text, query string) string {
    if query == "" {
        return text
    }
    tl := strings.ToLower(text)
    ql := strings.ToLower(query)
    q := []rune(ql)
    qi := 0
    var b strings.Builder
    tr := []rune(text)
    for i, ch := range tr {
        if qi < len(q) && []rune(tl)[i] == q[qi] {
            b.WriteString("{highlight}")
            b.WriteRune(ch)
            b.WriteString("{text}")
            qi++
        } else {
            b.WriteRune(ch)
        }
    }
    return b.String()
}

func (s *selector) handleSelection(td tryDir) {
    s.selected = &result{Type: "cd", Path: td.Path}
}

func (s *selector) handleCreateNew(fd int, st termState) {
    datePrefix := time.Now().Format("2006-01-02")
    if s.inputBuffer != "" {
        name := datePrefix + "-" + s.inputBuffer
        name = strings.ReplaceAll(name, " ", "-")
        s.selected = &result{Type: "mkdir", Path: filepath.Join(s.basePath, name)}
        return
    }
    // prompt in cooked mode
    uiPrint("{clear_screen}{home}", nil)
    uiPrint("{h2}Enter new try name{text}\r\n", nil)
    uiPrint("> {dim_text}"+datePrefix+"-{text}", nil)
    uiPrint("{show_cursor}", nil)
    os.Stdout.Sync()

    // restore cooked
    _ = setTermState(fd, &st.old)
    reader := bufio.NewReader(os.Stdin)
    line, _ := reader.ReadString('\n')
    // back to raw
    _, _ = enableRaw(fd)

    line = strings.TrimSpace(line)
    if line == "" {
        return
    }
    name := datePrefix + "-" + line
    name = strings.ReplaceAll(name, " ", "-")
    s.selected = &result{Type: "mkdir", Path: filepath.Join(s.basePath, name)}
}

func (s *selector) handleDelete(td tryDir, fd int, st termState) {
    // Get size and file count
    size := "???"
    if out, err := exec.Command("du", "-sh", td.Path).Output(); err == nil {
        parts := strings.Fields(string(out))
        if len(parts) > 0 {
            size = parts[0]
        }
    }

    files := "???"
    if out, err := exec.Command("sh", "-c", "find "+td.Path+" -type f | wc -l").Output(); err == nil {
        files = strings.TrimSpace(string(out))
    }

    // Show confirmation dialog
    uiPrint("{clear_screen}{home}", nil)
    uiPrint("{h2}Delete Directory{text}\r\n\r\n", nil)
    uiPrint("Are you sure you want to delete: {highlight}"+td.Basename+"{text}\r\n", nil)
    uiPrint("  {dim_text}in "+td.Path+"{text}\r\n", nil)
    uiPrint("  {dim_text}files: "+files+" files{text}\r\n", nil)
    uiPrint("  {dim_text}size: "+size+"{text}\r\n\r\n", nil)
    uiPrint("{highlight}Type {text}YES{highlight} to confirm: {text}", nil)
    uiPrint("{show_cursor}", nil)
    os.Stderr.Sync()

    // Restore cooked mode for input
    _ = setTermState(fd, &st.old)
    reader := bufio.NewReader(os.Stdin)
    line, _ := reader.ReadString('\n')
    // Back to raw
    _, _ = enableRaw(fd)
    uiPrint("{hide_cursor}", nil)

    confirmation := strings.TrimSpace(line)
    if confirmation == "YES" {
        if err := os.RemoveAll(td.Path); err != nil {
            s.deleteStatus = "Error: " + err.Error()
        } else {
            s.deleteStatus = "Deleted: " + td.Basename
            s.tries = nil // Clear cache to reload
        }
    } else {
        s.deleteStatus = "Delete cancelled"
    }
}

func getenv(k, def string) string {
    if v := os.Getenv(k); v != "" {
        return v
    }
    return def
}

func extractOptionWithValue(args *[]string, opt string) string {
    a := *args
    var val string
    for i := len(a) - 1; i >= 0; i-- {
        if a[i] == opt || strings.HasPrefix(a[i], opt+"=") {
            if strings.Contains(a[i], "=") {
                parts := strings.SplitN(a[i], "=", 2)
                val = parts[1]
            } else if i+1 < len(a) {
                val = a[i+1]
                // remove following value
                a = append(a[:i+1], a[i+2:]...)
            }
            // remove opt itself
            a = append(a[:i], a[i+1:]...)
            break
        }
    }
    *args = a
    return val
}

func main() {
    // Determine default path
    tryPath := getenv("TRY_PATH", filepath.Join(os.Getenv("HOME"), "src", "tries"))
    tryPath, _ = filepath.Abs(tryPath)

    // Global help
    for _, a := range os.Args[1:] {
        if a == "--help" || a == "-h" {
            printGlobalHelp(tryPath)
            os.Exit(0)
        }
    }

    if len(os.Args) < 2 {
        printGlobalHelp(tryPath)
        os.Exit(2)
    }

    args := append([]string{}, os.Args[1:]...)
    cmd := args[0]
    args = args[1:]
    pathOpt := extractOptionWithValue(&args, "--path")
    if pathOpt != "" {
        tryPath = pathOpt
    }
    tryPath, _ = filepath.Abs(tryPath)

    switch cmd {
    case "init":
        scriptPath, _ := filepath.Abs(os.Args[0])
        if len(args) > 0 && strings.HasPrefix(args[0], "/") {
            tryPath, _ = filepath.Abs(args[0])
            args = args[1:]
        }
        pathArg := ""
        if tryPath != "" {
            pathArg = " --path \"" + tryPath + "\""
        }

        // Check if fish shell
        shell := os.Getenv("SHELL")
        if strings.Contains(shell, "fish") {
            // Fish shell script
            fmt.Printf("function try\n")
            fmt.Printf("  set -l script_path \"%s\"\n", scriptPath)
            fmt.Printf("  # Check if first argument is a known command\n")
            fmt.Printf("  switch $argv[1]\n")
            fmt.Printf("    case clone worktree init\n")
            fmt.Printf("      set -l cmd (/usr/bin/env \"$script_path\"%s $argv 2>/dev/tty | string collect)\n", pathArg)
            fmt.Printf("    case '*'\n")
            fmt.Printf("      set -l cmd (/usr/bin/env \"$script_path\" cd%s $argv 2>/dev/tty | string collect)\n", pathArg)
            fmt.Printf("  end\n")
            fmt.Printf("  set -l rc $status\n")
            fmt.Printf("  if test $rc -eq 0\n")
            fmt.Printf("    if string match -r ' && ' -- $cmd\n")
            fmt.Printf("      eval $cmd\n")
            fmt.Printf("    else\n")
            fmt.Printf("      printf %%s $cmd\n")
            fmt.Printf("    end\n")
            fmt.Printf("  else\n")
            fmt.Printf("    printf %%s $cmd\n")
            fmt.Printf("  end\n")
            fmt.Printf("end\n")
        } else {
            // Bash/Zsh script
            fmt.Printf("try() {\n")
            fmt.Printf("  script_path='%s'\n", scriptPath)
            fmt.Printf("  # Check if first argument is a known command\n")
            fmt.Printf("  case \"$1\" in\n")
            fmt.Printf("    clone|worktree|init)\n")
            fmt.Printf("      cmd=$(/usr/bin/env \"$script_path\"%s \"$@\" 2>/dev/tty)\n", pathArg)
            fmt.Printf("      ;;\n")
            fmt.Printf("    *)\n")
            fmt.Printf("      cmd=$(/usr/bin/env \"$script_path\" cd%s \"$@\" 2>/dev/tty)\n", pathArg)
            fmt.Printf("      ;;\n")
            fmt.Printf("  esac\n")
            fmt.Printf("  rc=$?\n")
            fmt.Printf("  if [ $rc -eq 0 ]; then\n")
            fmt.Printf("    case \"$cmd\" in\n")
            fmt.Printf("      *\" && \"*) eval \"$cmd\" ;;\n")
            fmt.Printf("      *) printf %%s \"$cmd\" ;;\n")
            fmt.Printf("    esac\n")
            fmt.Printf("  else\n")
            fmt.Printf("    printf %%s \"$cmd\"\n")
            fmt.Printf("  fi\n")
            fmt.Printf("}\n")
        }

    case "clone":
        if len(args) == 0 {
            fmt.Fprintln(os.Stderr, "Error: git URI required for clone command")
            fmt.Fprintln(os.Stderr, "Usage: try clone <git-uri> [name]")
            os.Exit(1)
        }
        gitURI := args[0]
        customName := ""
        if len(args) > 1 {
            customName = strings.Join(args[1:], " ")
        }

        dirName := generateCloneDirectoryName(gitURI, customName)
        if dirName == "" {
            fmt.Fprintln(os.Stderr, "Error: Unable to parse git URI:", gitURI)
            os.Exit(1)
        }

        fullPath := filepath.Join(tryPath, dirName)
        tasks := []task{
            {Type: "target", Path: fullPath},
            {Type: "mkdir"},
            {Type: "echo", Msg: "Using {highlight}git clone{reset_fg} to create this trial from " + gitURI + "."},
            {Type: "git-clone", URI: gitURI},
            {Type: "touch"},
            {Type: "cd"},
        }
        emitTasksScript(tasks)

    case "worktree":
        if len(args) == 0 || args[0] == "dir" {
            // try worktree dir [name]
            var customName string
            if len(args) > 0 && args[0] == "dir" {
                customName = strings.Join(args[1:], " ")
            } else {
                customName = strings.Join(args, " ")
            }

            base := customName
            if base == "" {
                cwd, _ := os.Getwd()
                base = filepath.Base(cwd)
            }
            base = strings.ReplaceAll(base, " ", "-")

            datePrefix := time.Now().Format("2006-01-02")
            base = resolveUniqueNameWithVersioning(tryPath, datePrefix, base)
            dirName := datePrefix + "-" + base
            fullPath := filepath.Join(tryPath, dirName)

            tasks := []task{
                {Type: "target", Path: fullPath},
                {Type: "mkdir"},
            }

            // Check if CWD is a git repo
            if _, err := os.Stat(".git"); err == nil {
                cwd, _ := os.Getwd()
                tasks = append(tasks, task{Type: "echo", Msg: "Using {highlight}git worktree{reset_fg} to create this trial from " + cwd + "."})
                tasks = append(tasks, task{Type: "git-worktree"})
            }

            tasks = append(tasks, task{Type: "touch"}, task{Type: "cd"})
            emitTasksScript(tasks)
        } else {
            // try worktree <repo-path> [name]
            repoDir, _ := filepath.Abs(args[0])
            customName := ""
            if len(args) > 1 {
                customName = strings.Join(args[1:], " ")
            }

            base := customName
            if base == "" {
                base = filepath.Base(repoDir)
            }
            base = strings.ReplaceAll(base, " ", "-")

            datePrefix := time.Now().Format("2006-01-02")
            base = resolveUniqueNameWithVersioning(tryPath, datePrefix, base)
            dirName := datePrefix + "-" + base
            fullPath := filepath.Join(tryPath, dirName)

            tasks := []task{
                {Type: "target", Path: fullPath},
                {Type: "mkdir"},
                {Type: "echo", Msg: "Using {highlight}git worktree{reset_fg} to create this trial from " + repoDir + "."},
                {Type: "git-worktree", Repo: repoDir},
                {Type: "touch"},
                {Type: "cd"},
            }
            emitTasksScript(tasks)
        }

    case "cd":
        search := strings.Join(args, " ")

        // Support: try . [name] and try ./path [name]
        searchParts := strings.Fields(search)
        if len(searchParts) > 0 && strings.HasPrefix(searchParts[0], ".") {
            pathArg := searchParts[0]
            customName := ""
            if len(searchParts) > 1 {
                customName = strings.Join(searchParts[1:], " ")
            }

            repoDir, _ := filepath.Abs(pathArg)
            base := customName
            if base == "" {
                base = filepath.Base(repoDir)
            }
            base = strings.ReplaceAll(base, " ", "-")

            datePrefix := time.Now().Format("2006-01-02")
            base = resolveUniqueNameWithVersioning(tryPath, datePrefix, base)
            dirName := datePrefix + "-" + base
            fullPath := filepath.Join(tryPath, dirName)

            tasks := []task{
                {Type: "target", Path: fullPath},
                {Type: "mkdir"},
            }

            // Only add worktree when a .git directory exists at that path
            gitPath := filepath.Join(repoDir, ".git")
            if _, err := os.Stat(gitPath); err == nil {
                tasks = append(tasks, task{Type: "echo", Msg: "Using {highlight}git worktree{reset_fg} to create this trial from " + repoDir + "."})
                tasks = append(tasks, task{Type: "git-worktree", Repo: repoDir})
            }

            tasks = append(tasks, task{Type: "touch"}, task{Type: "cd"})
            emitTasksScript(tasks)
            return
        }

        // Git URL shorthand → clone workflow
        if len(searchParts) > 0 && isGitURI(searchParts[0]) {
            gitURI := searchParts[0]
            customName := ""
            if len(searchParts) > 1 {
                customName = strings.Join(searchParts[1:], " ")
            }

            dirName := generateCloneDirectoryName(gitURI, customName)
            if dirName == "" {
                fmt.Fprintln(os.Stderr, "Error: Unable to parse git URI:", gitURI)
                os.Exit(1)
            }

            fullPath := filepath.Join(tryPath, dirName)
            tasks := []task{
                {Type: "target", Path: fullPath},
                {Type: "mkdir"},
                {Type: "echo", Msg: "Using {highlight}git clone{reset_fg} to create this trial from " + gitURI + "."},
                {Type: "git-clone", URI: gitURI},
                {Type: "touch"},
                {Type: "cd"},
            }
            emitTasksScript(tasks)
        } else {
            // Regular interactive selector
            sel := newSelector(search, tryPath)
            res, _ := sel.run()
            if res != nil {
                tasks := []task{{Type: "target", Path: res.Path}}
                if res.Type == "mkdir" {
                    tasks = append(tasks, task{Type: "mkdir"})
                }
                tasks = append(tasks, task{Type: "touch"}, task{Type: "cd"})
                emitTasksScript(tasks)
            }
        }

    default:
        fmt.Fprintln(os.Stderr, "Unknown command:", cmd)
        printGlobalHelp(tryPath)
        os.Exit(2)
    }
}

func max(a, b int) int { if a > b { return a }; return b }
func min(a, b int) int { if a < b { return a }; return b }

// Git URI parsing
type gitInfo struct {
    User string
    Repo string
    Host string
}

func parseGitURI(uri string) *gitInfo {
    // Remove .git suffix if present
    uri = strings.TrimSuffix(uri, ".git")

    // https://github.com/user/repo
    if strings.Contains(uri, "github.com/") {
        parts := strings.Split(uri, "/")
        if len(parts) >= 2 {
            user := parts[len(parts)-2]
            repo := parts[len(parts)-1]
            return &gitInfo{User: user, Repo: repo, Host: "github.com"}
        }
    }

    // git@github.com:user/repo
    if strings.HasPrefix(uri, "git@") {
        parts := strings.SplitN(uri, ":", 2)
        if len(parts) == 2 {
            host := strings.TrimPrefix(parts[0], "git@")
            pathParts := strings.Split(parts[1], "/")
            if len(pathParts) >= 2 {
                user := pathParts[len(pathParts)-2]
                repo := pathParts[len(pathParts)-1]
                return &gitInfo{User: user, Repo: repo, Host: host}
            }
        }
    }

    // https://host/user/repo
    if strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://") {
        parts := strings.Split(uri, "/")
        if len(parts) >= 5 {
            host := parts[2]
            user := parts[3]
            repo := parts[4]
            return &gitInfo{User: user, Repo: repo, Host: host}
        }
    }

    return nil
}

func isGitURI(arg string) bool {
    if arg == "" {
        return false
    }
    return strings.HasPrefix(arg, "http://") ||
        strings.HasPrefix(arg, "https://") ||
        strings.HasPrefix(arg, "git@") ||
        strings.Contains(arg, "github.com") ||
        strings.Contains(arg, "gitlab.com") ||
        strings.HasSuffix(arg, ".git")
}

func generateCloneDirectoryName(gitURI, customName string) string {
    if customName != "" {
        return customName
    }

    parsed := parseGitURI(gitURI)
    if parsed == nil {
        return ""
    }

    datePrefix := time.Now().Format("2006-01-02")
    return fmt.Sprintf("%s-%s-%s", datePrefix, parsed.User, parsed.Repo)
}

// Directory versioning/uniqueness
func uniqueDirName(basePath, dirName string) string {
    candidate := dirName
    i := 2
    for {
        fullPath := filepath.Join(basePath, candidate)
        if _, err := os.Stat(fullPath); os.IsNotExist(err) {
            break
        }
        candidate = fmt.Sprintf("%s-%d", dirName, i)
        i++
    }
    return candidate
}

func resolveUniqueNameWithVersioning(basePath, datePrefix, base string) string {
    initial := datePrefix + "-" + base
    fullPath := filepath.Join(basePath, initial)
    if _, err := os.Stat(fullPath); os.IsNotExist(err) {
        return base
    }

    // Check if base ends with digits
    re := regexp.MustCompile(`^(.*?)(\d+)$`)
    if matches := re.FindStringSubmatch(base); matches != nil {
        stem := matches[1]
        n, _ := strconv.Atoi(matches[2])
        candidateNum := n + 1
        for {
            candidateBase := fmt.Sprintf("%s%d", stem, candidateNum)
            candidateFull := filepath.Join(basePath, datePrefix+"-"+candidateBase)
            if _, err := os.Stat(candidateFull); os.IsNotExist(err) {
                return candidateBase
            }
            candidateNum++
        }
    }

    // No numeric suffix; use -2 style uniqueness
    fullName := uniqueDirName(basePath, datePrefix+"-"+base)
    return strings.TrimPrefix(fullName, datePrefix+"-")
}

// Shell script emission
func shellQuote(s string) string {
    return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func emitTasksScript(tasks []task) {
    var targetPath string
    for _, t := range tasks {
        if t.Type == "target" {
            targetPath = t.Path
            break
        }
    }
    if targetPath == "" {
        return
    }

    var parts []string
    q := shellQuote(targetPath)

    for _, t := range tasks {
        switch t.Type {
        case "echo":
            if t.Msg != "" {
                // Expand tokens in message
                replacer := strings.NewReplacer(
                    "{highlight}", "\x1b[1;33m",
                    "{reset_fg}", "\x1b[39m",
                    "{text}", "\x1b[39m",
                )
                expanded := replacer.Replace(t.Msg)
                m := shellQuote(expanded)
                parts = append(parts, "echo "+m)
            }
        case "mkdir":
            parts = append(parts, "mkdir -p "+q)
        case "git-clone":
            parts = append(parts, "git clone "+shellQuote(t.URI)+" "+q)
        case "git-worktree":
            if t.Repo != "" {
                r := shellQuote(t.Repo)
                parts = append(parts, "/usr/bin/env sh -c 'if git -C "+r+" rev-parse --is-inside-work-tree >/dev/null 2>&1; then repo=$(git -C "+r+" rev-parse --show-toplevel); git -C \"$repo\" worktree add --detach "+q+" >/dev/null 2>&1 || true; fi; exit 0'")
            } else {
                parts = append(parts, "/usr/bin/env sh -c 'if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then repo=$(git rev-parse --show-toplevel); git -C \"$repo\" worktree add --detach "+q+" >/dev/null 2>&1 || true; fi; exit 0'")
            }
        case "touch":
            parts = append(parts, "touch "+q)
        case "cd":
            parts = append(parts, "cd "+q)
        }
    }

    fmt.Print(strings.Join(parts, " \\\n  && "))
}
