package tui

import (
	"context"
	_ "embed" // for embedding config.sh
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ddworken/hishtory/client/ai"
	"github.com/ddworken/hishtory/client/data"
	"github.com/ddworken/hishtory/client/hctx"
	"github.com/ddworken/hishtory/client/lib"
	"github.com/ddworken/hishtory/client/table"
	"github.com/ddworken/hishtory/client/tui/keybindings"
	"github.com/ddworken/hishtory/shared"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"golang.org/x/term"
)

var (
	CURRENT_QUERY_FOR_HIGHLIGHTING string = ""
	SELECTED_COMMAND               string = ""
)

// Globally shared monotonically increasing IDs used to prevent race conditions in handling async queries.
// If the user types 'l' and then 's', two queries will be dispatched: One for 'l' and one for 'ls'. These
// counters are used to ensure that we don't process the query results for 'ls' and then promptly overwrite
// them with the results for 'l'.
var (
	LAST_DISPATCHED_QUERY_ID        = 0
	LAST_DISPATCHED_QUERY_TIMESTAMP time.Time
	LAST_PROCESSED_QUERY_ID         = -1
)

type SelectStatus int64

const (
	NotSelected SelectStatus = iota
	Selected
	SelectedWithChangeDir
)

var loadedKeyBindings keybindings.KeyMap = keybindings.DefaultKeyMap

type model struct {
	// context
	ctx context.Context

	// Model for the loading spinner.
	spinner spinner.Model
	// Whether data is still loading and the spinner should still be displayed.
	isLoading bool

	// Model for the help bar at the bottom of the page
	help help.Model

	// Whether the TUI is quitting.
	quitting bool

	// The table used for displaying search results. Nil if the initial search query hasn't returned yet.
	table *table.Model
	// The entries in the table
	tableEntries []*data.HistoryEntry
	// Whether the user has hit enter to select an entry and the TUI is thus about to quit.
	selected SelectStatus

	// The search box for the query
	queryInput textinput.Model
	// The query to run. Reset to nil after it was run.
	runQuery *string
	// The previous query that was run.
	lastQuery string

	// Unrecoverable error.
	fatalErr error
	// An error while searching. Recoverable and displayed as a warning message.
	searchErr error
	// Whether the device is offline. If so, a warning will be displayed.
	isOffline bool

	// A banner from the backend to be displayed. Generally an empty string.
	banner string

	// The currently executing shell. Defaults to bash if not specified. Used for more precise AI suggestions.
	shellName string

	// Whether we've finished the first load of results. If we haven't, we refuse to run additional queries to avoid race conditions with how we handle invalid initial queries.
	hasFinishedFirstLoad bool
}

type (
	doneDownloadingMsg struct{}
	offlineMsg         struct{}
	bannerMsg          struct {
		banner string
	}
)

type asyncQueryFinishedMsg struct {
	// The query ID finished running. Used to ensure that we only process this message if it is the latest query to finish.
	queryId int
	// The table rows and entries
	rows    []table.Row
	entries []*data.HistoryEntry
	// An error from searching, if one occurred
	searchErr error
	// Whether to force a full refresh of the table
	forceUpdateTable bool
	// Whether to maintain the cursor position
	maintainCursor bool
	// An updated search query. May be used for initial queries when they're invalid.
	overriddenSearchQuery *string

	isFirstQuery bool
}

func initialModel(ctx context.Context, shellName, initialQuery string) model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
	queryInput := textinput.New()
	cfg := hctx.GetConf(ctx)
	defaultFilter := cfg.DefaultFilter
	if defaultFilter != "" {
		queryInput.Prompt = "[" + defaultFilter + "] "
	}
	queryInput.PromptStyle = queryInput.PlaceholderStyle
	if defaultFilter == "" {
		queryInput.Placeholder = "ls"
	}
	queryInput.Focus()
	queryInput.CharLimit = 200
	width, _, err := getTerminalSize()
	if err == nil {
		queryInput.Width = width
	} else {
		hctx.GetLogger().Warnf("getTerminalSize() return err=%#v, defaulting queryInput to a width of 50", err)
		queryInput.Width = 50
	}
	if initialQuery != "" {
		queryInput.SetValue(initialQuery)
	}
	CURRENT_QUERY_FOR_HIGHLIGHTING = initialQuery
	return model{ctx: ctx, spinner: s, isLoading: true, table: nil, tableEntries: []*data.HistoryEntry{}, runQuery: &initialQuery, queryInput: queryInput, help: help.New(), shellName: shellName, hasFinishedFirstLoad: false}
}

func (m model) Init() tea.Cmd {
	return m.spinner.Tick
}

func updateTable(m model, rows []table.Row, entries []*data.HistoryEntry, searchErr error, forceUpdateTable, maintainCursor bool) model {
	if m.runQuery == nil {
		m.runQuery = &m.lastQuery
	}
	m.searchErr = searchErr
	if searchErr != nil {
		return m
	}
	m.tableEntries = entries
	initialCursor := 0
	if m.table != nil {
		initialCursor = m.table.Cursor()
	}
	if forceUpdateTable || m.table == nil {
		t, err := makeTable(m.ctx, m.shellName, rows)
		if err != nil {
			m.fatalErr = err
			return m
		}
		m.table = &t
	}
	m.table.SetRows(rows)
	if maintainCursor {
		m.table.SetCursor(initialCursor)
	} else {
		m.table.SetCursor(0)
	}
	m.lastQuery = *m.runQuery
	m.runQuery = nil
	preventTableOverscrolling(m)
	return m
}

func preventTableOverscrolling(m model) {
	if m.table != nil {
		if m.table.Cursor() >= len(m.tableEntries) {
			// Ensure that we can't scroll past the end of the table
			m.table.SetCursor(len(m.tableEntries) - 1)
		}
	}
}

func runQueryAndUpdateTable(m model, forceUpdateTable, maintainCursor bool) tea.Cmd {
	if (m.runQuery != nil && *m.runQuery != m.lastQuery) || forceUpdateTable || m.searchErr != nil {
		query := m.lastQuery
		if m.runQuery != nil {
			query = *m.runQuery
		}
		queryId := allocateQueryId()
		conf := hctx.GetConf(m.ctx)
		defaultFilter := conf.DefaultFilter
		if m.queryInput.Prompt == "" {
			// The default filter was cleared for this session, so don't apply it
			defaultFilter = ""
		}

		// Kick off an async query to getRows() so that we can start our DB query in the background
		// before bubbletea actually invokes our tea.Msg. This reduces latency between key presses
		// and results being displayed.
		go func() {
			_, _, _ = getRows(m.ctx, conf.DisplayedColumns, m.shellName, defaultFilter, query, getNumEntriesNeeded(m.ctx))
		}()

		return func() tea.Msg {
			rows, entries, searchErr := getRows(m.ctx, conf.DisplayedColumns, m.shellName, defaultFilter, query, getNumEntriesNeeded(m.ctx))
			return asyncQueryFinishedMsg{queryId, rows, entries, searchErr, forceUpdateTable, maintainCursor, nil, false}
		}
	}
	return nil
}

func sanitizeEscapeCodes(input string) string {
	re := regexp.MustCompile(`\d\d;rgb:[0-9a-f]{4}/[0-9a-f]{4}/[0-9a-f]{4}`)
	return re.ReplaceAllString(input, "")
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.queryInput.SetValue(sanitizeEscapeCodes(m.queryInput.Value()))
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, loadedKeyBindings.Quit):
			m.quitting = true
			return m, tea.Quit
		case key.Matches(msg, loadedKeyBindings.SelectEntry):
			if len(m.tableEntries) != 0 && m.table != nil {
				m.selected = Selected
			}
			return m, tea.Quit
		case key.Matches(msg, loadedKeyBindings.SelectEntryAndChangeDir):
			if len(m.tableEntries) != 0 && m.table != nil {
				m.selected = SelectedWithChangeDir
			}
			return m, tea.Quit
		case key.Matches(msg, loadedKeyBindings.DeleteEntry):
			if m.table == nil {
				return m, nil
			}
			err := deleteHistoryEntry(m.ctx, *m.tableEntries[m.table.Cursor()])
			if err != nil {
				m.fatalErr = err
				return m, nil
			}
			cmd := runQueryAndUpdateTable(m, true, true)
			preventTableOverscrolling(m)
			return m, cmd
		case key.Matches(msg, loadedKeyBindings.Help):
			m.help.ShowAll = !m.help.ShowAll
			return m, nil
		case key.Matches(msg, loadedKeyBindings.JumpStartOfInput):
			m.queryInput.SetCursor(0)
			return m, nil
		case key.Matches(msg, loadedKeyBindings.JumpEndOfInput):
			m.queryInput.SetCursor(len(m.queryInput.Value()))
			return m, nil
		case key.Matches(msg, loadedKeyBindings.WordLeft):
			wordBoundaries := calculateWordBoundaries(m.queryInput.Value())
			lastBoundary := 0
			for _, boundary := range wordBoundaries {
				if boundary >= m.queryInput.Position() {
					m.queryInput.SetCursor(lastBoundary)
					break
				}
				lastBoundary = boundary
			}
			return m, nil
		case key.Matches(msg, loadedKeyBindings.WordRight):
			wordBoundaries := calculateWordBoundaries(m.queryInput.Value())
			for _, boundary := range wordBoundaries {
				if boundary > m.queryInput.Position() {
					m.queryInput.SetCursor(boundary)
					break
				}
			}
			return m, nil
		default:
			pendingCommands := tea.Batch()
			if m.table != nil {
				t, cmd1 := m.table.Update(msg)
				m.table = &t
				if strings.HasPrefix(msg.String(), "alt+") {
					return m, tea.Batch(cmd1)
				}
				pendingCommands = tea.Batch(pendingCommands, cmd1)
			}
			forceUpdateTable := false
			if msg.String() == "backspace" && (m.queryInput.Value() == "" || m.queryInput.Position() == 0) {
				// Handle deleting the default filter just for this TUI instance
				m.queryInput.Prompt = ""
				forceUpdateTable = true
			}
			i, cmd2 := m.queryInput.Update(msg)
			m.queryInput = i
			searchQuery := m.queryInput.Value()
			m.runQuery = &searchQuery
			CURRENT_QUERY_FOR_HIGHLIGHTING = searchQuery
			cmd3 := runQueryAndUpdateTable(m, forceUpdateTable, false)
			preventTableOverscrolling(m)
			return m, tea.Batch(pendingCommands, cmd2, cmd3)
		}
	case tea.WindowSizeMsg:
		m.help.Width = msg.Width
		m.queryInput.Width = msg.Width
		cmd := runQueryAndUpdateTable(m, true, true)
		return m, cmd
	case offlineMsg:
		m.isOffline = true
		return m, nil
	case bannerMsg:
		m.banner = msg.banner
		return m, nil
	case doneDownloadingMsg:
		m.isLoading = false
		return m, nil
	case asyncQueryFinishedMsg:
		if msg.queryId > LAST_PROCESSED_QUERY_ID {
			LAST_PROCESSED_QUERY_ID = msg.queryId
			m = updateTable(m, msg.rows, msg.entries, msg.searchErr, msg.forceUpdateTable, msg.maintainCursor)
			if msg.overriddenSearchQuery != nil {
				m.queryInput.SetValue(*msg.overriddenSearchQuery)
			}
		}
		if msg.isFirstQuery {
			m.hasFinishedFirstLoad = true
		}
		return m, nil
	default:
		var cmd tea.Cmd
		if m.isLoading {
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		} else {
			if m.table != nil {
				t, cmd := m.table.Update(msg)
				m.table = &t
				return m, cmd
			}
			return m, nil
		}
	}
}

func calculateWordBoundaries(input string) []int {
	ret := make([]int, 0)
	ret = append(ret, 0)
	prevWasBreaking := false
	for idx, char := range input {
		if char == ' ' || char == '-' {
			if !prevWasBreaking {
				ret = append(ret, idx)
			}
			prevWasBreaking = true
		} else {
			prevWasBreaking = false
		}
	}
	if !prevWasBreaking {
		ret = append(ret, len(input))
	}
	return ret
}

func (m model) View() string {
	if m.fatalErr != nil {
		return fmt.Sprintf("An unrecoverable error occured: %v\n", m.fatalErr)
	}
	if m.selected == Selected || m.selected == SelectedWithChangeDir {
		SELECTED_COMMAND = m.tableEntries[m.table.Cursor()].Command
		if m.selected == SelectedWithChangeDir {
			changeDir := m.tableEntries[m.table.Cursor()].CurrentWorkingDirectory
			if strings.HasPrefix(changeDir, "~/") {
				homedir, err := os.UserHomeDir()
				if err != nil {
					hctx.GetLogger().Warnf("UserHomeDir() return err=%v, skipping replacing ~/", err)
				} else {
					strippedChangeDir, _ := strings.CutPrefix(changeDir, "~/")
					changeDir = filepath.Join(homedir, strippedChangeDir)
				}
			}
			SELECTED_COMMAND = "cd \"" + changeDir + "\" && " + SELECTED_COMMAND
		}
		return ""
	}
	if m.quitting {
		return ""
	}
	additionalMessages := make([]string, 0)
	if m.isLoading {
		additionalMessages = append(additionalMessages, fmt.Sprintf("%s Loading hishtory entries from other devices...", m.spinner.View()))
	}
	if m.isOffline {
		additionalMessages = append(additionalMessages, "Warning: failed to contact the hishtory backend (are you offline?), so some results may be stale")
	}
	if m.searchErr != nil {
		additionalMessages = append(additionalMessages, fmt.Sprintf("Warning: failed to search: %v", m.searchErr))
	}
	if LAST_PROCESSED_QUERY_ID < LAST_DISPATCHED_QUERY_ID && time.Since(LAST_DISPATCHED_QUERY_TIMESTAMP) > time.Second {
		additionalMessages = append(additionalMessages, fmt.Sprintf("%s Executing search query...", m.spinner.View()))
	}
	additionalMessagesStr := strings.Join(additionalMessages, "\n") + "\n"
	if isExtraCompactHeightMode(m.ctx) {
		additionalMessagesStr = "\n"
	}
	helpView := m.help.View(loadedKeyBindings)
	if isExtraCompactHeightMode(m.ctx) {
		helpView = ""
	}
	additionalSpacing := "\n"
	if isCompactHeightMode(m.ctx) {
		additionalSpacing = ""
	}
	return fmt.Sprintf("%s%s%s%sSearch Query: %s\n%s%s\n", additionalSpacing, additionalMessagesStr, m.banner, additionalSpacing, m.queryInput.View(), additionalSpacing, renderNullableTable(m, helpView)) + helpView
}

func isExtraCompactHeightMode(ctx context.Context) bool {
	if hctx.GetConf(ctx).ForceCompactMode {
		return true
	}
	_, height, err := getTerminalSize()
	if err != nil {
		hctx.GetLogger().Warnf("got err=%v when retrieving terminal dimensions, assuming the terminal is reasonably tall", err)
		return false
	}
	return height < 15
}

func isCompactHeightMode(ctx context.Context) bool {
	if hctx.GetConf(ctx).ForceCompactMode {
		return true
	}
	_, height, err := getTerminalSize()
	if err != nil {
		hctx.GetLogger().Warnf("got err=%v when retrieving terminal dimensions, assuming the terminal is reasonably tall", err)
		return false
	}
	return height < 25
}

func getBaseStyle(config hctx.ClientConfig) lipgloss.Style {
	return lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color(config.ColorScheme.BorderColor))
}

func renderNullableTable(m model, helpText string) string {
	if m.table == nil {
		return strings.Repeat("\n", getTableHeight(m.ctx)+3)
	}
	helpTextLen := strings.Count(helpText, "\n")
	baseStyle := getBaseStyle(*hctx.GetConf(m.ctx))
	if isCompactHeightMode(m.ctx) && helpTextLen > 1 {
		// If the help text is expanded, and this is a small window, then we truncate the table so that the help text displays on top of it
		lines := strings.Split(baseStyle.Render(m.table.View()), "\n")
		truncated := lines[:len(lines)-helpTextLen]
		return strings.Join(truncated, "\n")
	}
	return baseStyle.Render(m.table.View())
}

func getRowsFromAiSuggestions(ctx context.Context, columnNames []string, shellName, query string) ([]table.Row, []*data.HistoryEntry, error) {
	suggestions, err := ai.DebouncedGetAiSuggestions(ctx, shellName, strings.TrimPrefix(query, "?"), 5)
	if err != nil {
		hctx.GetLogger().Warnf("failed to get AI query suggestions: %v", err)
		return nil, nil, fmt.Errorf("failed to get AI query suggestions: %w", err)
	}
	var rows []table.Row
	var entries []*data.HistoryEntry
	for _, suggestion := range suggestions {
		entry := data.HistoryEntry{
			LocalUsername:           "OpenAI",
			Hostname:                "OpenAI",
			Command:                 suggestion,
			CurrentWorkingDirectory: "N/A",
			HomeDirectory:           "N/A",
			ExitCode:                0,
			StartTime:               time.Unix(0, 0).UTC(),
			EndTime:                 time.Unix(0, 0).UTC(),
			DeviceId:                "OpenAI",
			EntryId:                 "OpenAI",
		}
		entries = append(entries, &entry)
		row, err := lib.BuildTableRow(ctx, columnNames, entry, func(s string) string { return s })
		if err != nil {
			return nil, nil, fmt.Errorf("failed to build row for entry=%#v: %w", entry, err)
		}
		rows = append(rows, row)
	}
	hctx.GetLogger().Infof("getRowsFromAiSuggestions(%#v) ==> %#v", query, suggestions)
	return rows, entries, nil
}

func TestOnlyGetRows(ctx context.Context, columnNames []string, shellName, defaultFilter, query string, numEntries int) ([]table.Row, []*data.HistoryEntry, error) {
	return getRows(ctx, columnNames, shellName, defaultFilter, query, numEntries)
}

func getRows(ctx context.Context, columnNames []string, shellName, defaultFilter, query string, numEntries int) ([]table.Row, []*data.HistoryEntry, error) {
	db := hctx.GetDb(ctx)
	config := hctx.GetConf(ctx)
	if config.AiCompletion && strings.HasPrefix(query, "?") && len(query) > 1 {
		return getRowsFromAiSuggestions(ctx, columnNames, shellName, query)
	}
	searchResults, err := lib.SearchWithCache(ctx, db, defaultFilter+" "+query, numEntries)
	if err != nil {
		return nil, nil, err
	}
	var rows []table.Row
	var filteredData []*data.HistoryEntry
	seenCommands := make(map[string]bool)

	for i := 0; i < numEntries; i++ {
		if i < len(searchResults) {
			entry := searchResults[i]

			if config.FilterDuplicateCommands && entry != nil {
				cmd := strings.TrimSpace(entry.Command)
				if seenCommands[cmd] {
					continue
				}
				seenCommands[cmd] = true
			}

			row, err := lib.BuildTableRow(ctx, columnNames, *entry, commandEscaper)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to build row for entry=%#v: %w", entry, err)
			}
			rows = append(rows, row)
			filteredData = append(filteredData, entry)
		} else {
			rows = append(rows, table.Row{})
		}
	}
	return rows, filteredData, nil
}

func commandEscaper(cmd string) string {
	if !strings.Contains(cmd, "\n") && !strings.Contains(cmd, "\t") {
		// No special escaping necessary
		return cmd
	}
	return fmt.Sprintf("%#v", cmd)
}

func calculateColumnWidths(rows []table.Row, numColumns int) []int {
	neededColumnWidth := make([]int, numColumns)
	for _, row := range rows {
		for i, v := range row {
			neededColumnWidth[i] = max(neededColumnWidth[i], len(v))
		}
	}
	return neededColumnWidth
}

func getTerminalSize() (int, int, error) {
	return term.GetSize(2)
}

var bigQueryResults []table.Row

func makeTableColumns(ctx context.Context, shellName string, columnNames []string, rows []table.Row) ([]table.Column, error) {
	// Handle an initial query with no results
	if len(rows) == 0 || len(rows[0]) == 0 {
		allRows, _, err := getRows(ctx, columnNames, shellName, hctx.GetConf(ctx).DefaultFilter, "", 25)
		if err != nil {
			return nil, err
		}
		if len(allRows) == 0 || len(allRows[0]) == 0 {
			// There are truly zero history entries. Let's still display a table in this case rather than erroring out.
			allRows = make([]table.Row, 0)
			row := make([]string, 0)
			for range columnNames {
				row = append(row, " ")
			}
			allRows = append(allRows, row)
		}
		return makeTableColumns(ctx, shellName, columnNames, allRows)
	}

	// Calculate the minimum amount of space that we need for each column for the current actual search
	columnWidths := calculateColumnWidths(rows, len(columnNames))
	totalWidth := (len(columnWidths) + 1) * 2 // The amount of space needed for the table padding
	for i, name := range columnNames {
		columnWidths[i] = max(columnWidths[i], len(name))
		totalWidth += columnWidths[i]
	}

	// Calculate the maximum column width that is useful for each column if we search for the empty string
	if bigQueryResults == nil {
		bigRows, _, err := getRows(ctx, columnNames, shellName, "", "", 1000)
		if err != nil {
			return nil, err
		}
		bigQueryResults = bigRows
	}
	maximumColumnWidths := calculateColumnWidths(bigQueryResults, len(columnNames))

	// Get the actual terminal width. If we're below this, opportunistically add some padding aiming for the maximum column widths
	terminalWidth, _, err := getTerminalSize()
	if err != nil {
		return nil, fmt.Errorf("failed to get terminal size: %w", err)
	}
	for totalWidth < (terminalWidth - len(columnNames)) {
		prevTotalWidth := totalWidth
		for i := range columnNames {
			if columnWidths[i] < maximumColumnWidths[i]+5 {
				columnWidths[i] += 1
				totalWidth += 1
			}
		}
		if totalWidth == prevTotalWidth {
			break
		}
	}

	// And if we are too large from the initial query, let's shrink things to make the table fit. We'll use the heuristic of always shrinking the widest column.
	for totalWidth > terminalWidth {
		largestColumnIdx := -1
		largestColumnSize := -1
		for i := range columnNames {
			if columnWidths[i] > largestColumnSize {
				largestColumnIdx = i
				largestColumnSize = columnWidths[i]
			}
		}
		columnWidths[largestColumnIdx] -= 1
		totalWidth -= 1
	}

	// And finally, create some actual columns!
	columns := make([]table.Column, 0)
	for i, name := range columnNames {
		columns = append(columns, table.Column{Title: name, Width: columnWidths[i]})
	}
	return columns, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func getTableHeight(ctx context.Context) int {
	config := hctx.GetConf(ctx)
	if config.FullScreenRendering {
		_, terminalHeight, err := getTerminalSize()
		if err != nil {
			// A reasonable guess at a default if for some reason we fail to retrieve the terminal size
			return 30
		}
		return max(terminalHeight-15, 20)
	}
	// Default to 20 when not full-screen since we want to balance showing a large table with not using the entire screen
	return 20
}

func getNumEntriesNeeded(ctx context.Context) int {
	// Get more than table height since the TUI filters some out (e.g. duplicate entries)
	return getTableHeight(ctx) * 5
}

func makeTable(ctx context.Context, shellName string, rows []table.Row) (table.Model, error) {
	config := hctx.GetConf(ctx)
	columns, err := makeTableColumns(ctx, shellName, config.DisplayedColumns, rows)
	if err != nil {
		return table.Model{}, err
	}
	km := table.KeyMap{
		LineUp:   loadedKeyBindings.Up,
		LineDown: loadedKeyBindings.Down,
		PageUp:   loadedKeyBindings.PageUp,
		PageDown: loadedKeyBindings.PageDown,
		GotoTop: key.NewBinding(
			key.WithKeys("home"),
			key.WithHelp("home", "go to start"),
		),
		GotoBottom: key.NewBinding(
			key.WithKeys("end"),
			key.WithHelp("end", "go to end"),
		),
		MoveLeft:  loadedKeyBindings.TableLeft,
		MoveRight: loadedKeyBindings.TableRight,
	}
	_, terminalHeight, err := getTerminalSize()
	if err != nil {
		return table.Model{}, err
	}
	tuiSize := 12
	if isCompactHeightMode(ctx) {
		tuiSize -= 2
	}
	if isExtraCompactHeightMode(ctx) {
		tuiSize -= 3
	}
	tableHeight := min(getTableHeight(ctx), terminalHeight-tuiSize)
	t := table.New(
		table.WithColumns(columns),
		table.WithRows(rows),
		table.WithFocused(true),
		table.WithHeight(tableHeight),
		table.WithKeyMap(km),
	)

	s := table.DefaultStyles()
	s.Header = s.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color(config.ColorScheme.BorderColor)).
		BorderBottom(true).
		Bold(false)
	s.Selected = s.Selected.
		Foreground(lipgloss.Color(config.ColorScheme.SelectedText)).
		Background(lipgloss.Color(config.ColorScheme.SelectedBackground)).
		Bold(false)
	if config.HighlightMatches {
		MATCH_NOTHING_REGEXP := regexp.MustCompile("a^")
		s.RenderCell = func(model table.Model, value string, position table.CellPosition) string {
			var re *regexp.Regexp
			CURRENT_QUERY_FOR_HIGHLIGHTING = strings.TrimSpace(CURRENT_QUERY_FOR_HIGHLIGHTING)
			if CURRENT_QUERY_FOR_HIGHLIGHTING == "" {
				// If there is no search query, then there is nothing to highlight
				re = MATCH_NOTHING_REGEXP
			} else {
				queryRegex := lib.MakeRegexFromQuery(CURRENT_QUERY_FOR_HIGHLIGHTING)
				r, err := regexp.Compile(queryRegex)
				if err != nil {
					// Failed to compile the regex for highlighting matches, this should never happen. In this
					// case, just use a regexp that matches nothing to ensure that the TUI doesn't crash.
					hctx.GetLogger().Warnf("Failed to compile regex %#v for query %#v, disabling highlighting of matches", queryRegex, CURRENT_QUERY_FOR_HIGHLIGHTING)
					re = MATCH_NOTHING_REGEXP
				} else {
					re = r
				}
			}

			// func to render a given chunk of `value`. `isMatching` is whether `v` matches the search query (and
			// thus needs to be highlighted). `isLeftMost` and `isRightMost` determines whether additional
			// padding is added (to reproduce the padding that `s.Cell` normally adds).
			renderChunk := func(v string, isMatching, isLeftMost, isRightMost bool) string {
				chunkStyle := lipgloss.NewStyle()
				if position.IsRowSelected {
					// Apply the selected style as the base style if this is the highlighted row of the table
					chunkStyle = s.Selected
				}
				if isLeftMost {
					chunkStyle = chunkStyle.PaddingLeft(1)
				}
				if isRightMost {
					chunkStyle = chunkStyle.PaddingRight(1)
				}
				if isMatching {
					chunkStyle = chunkStyle.Bold(true)
				}
				return chunkStyle.Render(v)
			}

			matches := re.FindAllStringIndex(value, -1)
			if len(matches) == 0 {
				// No matches, so render the entire value
				return renderChunk(value /*isMatching = */, false /*isLeftMost = */, true /*isRightMost = */, true)
			}

			// Iterate through the chunks of the value and highlight the relevant pieces
			ret := ""
			lastIncludedIdx := 0
			for _, match := range re.FindAllStringIndex(value, -1) {
				matchStartIdx := match[0]
				matchEndIdx := match[1]
				beforeMatch := value[lastIncludedIdx:matchStartIdx]
				match := value[matchStartIdx:matchEndIdx]
				if beforeMatch != "" {
					ret += renderChunk(beforeMatch, false, lastIncludedIdx == 0, lastIncludedIdx+1 == len(value))
				}
				if match != "" {
					ret += renderChunk(match, true, matchStartIdx == 0, matchEndIdx == len(value))
				}
				lastIncludedIdx = matchEndIdx
			}
			if lastIncludedIdx != len(value) {
				ret += renderChunk(value[lastIncludedIdx:], false, false, true)
			}
			return ret
		}
	}
	t.SetStyles(s)
	t.Focus()
	return t, nil
}

func deleteHistoryEntry(ctx context.Context, entry data.HistoryEntry) error {
	db := hctx.GetDb(ctx)
	// Delete locally
	r := db.Model(&data.HistoryEntry{}).Where("device_id = ? AND end_time = ?", entry.DeviceId, entry.EndTime).Delete(&data.HistoryEntry{})
	if r.Error != nil {
		return r.Error
	}

	// Delete remotely
	config := hctx.GetConf(ctx)
	if config.IsOffline {
		return nil
	}
	dr := shared.DeletionRequest{
		UserId:   data.UserId(hctx.GetConf(ctx).UserSecret),
		SendTime: time.Now(),
	}
	dr.Messages.Ids = append(dr.Messages.Ids,
		shared.MessageIdentifier{DeviceId: entry.DeviceId, EndTime: entry.EndTime, EntryId: entry.EntryId},
	)
	err := lib.SendDeletionRequest(ctx, dr)
	if err != nil {
		return err
	}

	return lib.ClearSearchCache(ctx)
}

func configureColorProfile(ctx context.Context) {
	if hctx.GetConf(ctx).ColorScheme == hctx.GetDefaultColorScheme() {
		// Set termenv.ANSI for the default color scheme, so that we preserve
		// the true default color scheme of hishtory which was initially
		// configured with termenv.ANSI (even though we want to support
		// full colors) for custom color schemes.
		lipgloss.SetColorProfile(termenv.ANSI)
		return
	}
	if os.Getenv("HISHTORY_TEST") != "" {
		// We also set termenv.ANSI for tests so as to ensure that all our
		// test environments behave the same (by default, github actions
		// ubuntu and macos have different termenv support).
		lipgloss.SetColorProfile(termenv.ANSI)
		return
	}
	// When the shell launches control-R it isn't hooked up to the main TTY,
	// which means that termenv isn't able to accurately detect color support
	// in the current terminal. We set the environment variable _hishtory_tui_color
	// to an int representing the termenv. If it is unset or set to 0, then we don't
	// know the current color support, and we have to guess it. This means we
	// risk either:
	//   * Choosing too high of a color support, and breaking hishtory colors
	//     in certain terminals
	//   * Choosing too low of a color support, and ending up with truncating
	//     customized colors
	//
	// The default terminal app on MacOS only supports termenv.ANSI256 (8 bit
	// colors), which means we likely shouldn't default to TrueColor. From
	// my own digging, I can't find any modern terminals that don't support
	// termenv.ANSI256, so it seems like a reasonable default here.
	colorProfileStr := os.Getenv("_hishtory_tui_color")
	if colorProfileStr == "" {
		// Fall back to the default
		lipgloss.SetColorProfile(termenv.ANSI256)
		return
	}
	colorProfileInt, err := strconv.Atoi(colorProfileStr)
	if err != nil {
		colorProfileInt = 0
	}
	// The int mappings for this are defined in query.go
	switch colorProfileInt {
	case 1:
		lipgloss.SetColorProfile(termenv.TrueColor)
	case 2:
		lipgloss.SetColorProfile(termenv.ANSI256)
	case 3:
		lipgloss.SetColorProfile(termenv.ANSI)
	case 4:
		lipgloss.SetColorProfile(termenv.Ascii)
	default:
		fallthrough
	case 0:
		// Unknown, so fall back to the default
		lipgloss.SetColorProfile(termenv.ANSI256)
	}
}

func buildInitialQueryWithSearchEscaping(initialQueryArray []string) (string, error) {
	var initialQuery string

	for i, queryChunk := range initialQueryArray {
		if i != 0 {
			initialQuery += " "
		}
		if strings.HasPrefix(queryChunk, "-") {
			quoted, err := json.Marshal(queryChunk)
			if err != nil {
				return "", fmt.Errorf("failed to marshal query chunk for escaping: %w", err)
			}
			initialQuery += string(quoted)
		} else {
			initialQuery += queryChunk
		}
	}

	return initialQuery, nil
}

func splitQueryArray(initialQueryArray []string) []string {
	var splitQueryArray []string
	for _, queryChunk := range initialQueryArray {
		splitQueryArray = append(splitQueryArray, strings.Split(queryChunk, " ")...)
	}
	return splitQueryArray
}

func allocateQueryId() int {
	LAST_DISPATCHED_QUERY_ID++
	LAST_DISPATCHED_QUERY_TIMESTAMP = time.Now()
	return LAST_DISPATCHED_QUERY_ID
}

func TuiQuery(ctx context.Context, shellName string, initialQueryArray []string) error {
	initialQueryArray = splitQueryArray(initialQueryArray)
	initialQueryWithEscaping, err := buildInitialQueryWithSearchEscaping(initialQueryArray)
	if err != nil {
		return err
	}
	loadedKeyBindings = hctx.GetConf(ctx).KeyBindings.ToKeyMap()
	configureColorProfile(ctx)
	additionalOptions := []tea.ProgramOption{tea.WithOutput(os.Stderr)}
	if hctx.GetConf(ctx).FullScreenRendering {
		additionalOptions = append(additionalOptions, tea.WithAltScreen())
	}
	p := tea.NewProgram(initialModel(ctx, shellName, initialQueryWithEscaping), additionalOptions...)
	// Async: Get the initial set of rows
	go func() {
		queryId := allocateQueryId()
		conf := hctx.GetConf(ctx)
		rows, entries, err := getRows(ctx, conf.DisplayedColumns, shellName, conf.DefaultFilter, initialQueryWithEscaping, getNumEntriesNeeded(ctx))
		if err == nil || initialQueryWithEscaping == "" {
			if err != nil {
				panic(err)
			}
			p.Send(asyncQueryFinishedMsg{queryId: queryId, rows: rows, entries: entries, searchErr: err, forceUpdateTable: true, maintainCursor: false, overriddenSearchQuery: nil, isFirstQuery: true})
		} else {
			// The initial query is likely invalid in some way, let's just drop it
			emptyQuery := ""
			rows, entries, err := getRows(ctx, hctx.GetConf(ctx).DisplayedColumns, shellName, conf.DefaultFilter, emptyQuery, getNumEntriesNeeded(ctx))
			if err != nil {
				panic(err)
			}
			p.Send(asyncQueryFinishedMsg{queryId: allocateQueryId(), rows: rows, entries: entries, searchErr: err, forceUpdateTable: true, maintainCursor: false, overriddenSearchQuery: &emptyQuery, isFirstQuery: true})
		}
	}()
	// Async: Retrieve additional entries from the backend
	go func() {
		err := lib.RetrieveAdditionalEntriesFromRemote(ctx, "tui")
		if err != nil {
			p.Send(err)
		}
		p.Send(doneDownloadingMsg{})
	}()
	// Async: Process deletion requests
	go func() {
		err := lib.ProcessDeletionRequests(ctx)
		if err != nil {
			p.Send(err)
		}
	}()
	// Async: Check for any banner from the server
	go func() {
		banner, err := lib.GetBanner(ctx)
		if err != nil {
			if lib.IsOfflineError(ctx, err) {
				p.Send(offlineMsg{})
			} else {
				p.Send(err)
			}
		}
		p.Send(bannerMsg{banner: string(banner)})
	}()
	// Blocking: Start the TUI
	_, err = p.Run()
	if err != nil {
		return err
	}
	if SELECTED_COMMAND == "" && os.Getenv("HISHTORY_TERM_INTEGRATION") != "" {
		// Print out the initialQuery instead so that we don't clear the terminal (note that we don't use the escaped one here)
		SELECTED_COMMAND = strings.Join(initialQueryArray, " ")
	}
	fmt.Printf("%s\n", SELECTED_COMMAND)
	return nil
}

// TODO: support custom key bindings
// TODO: make the help page wrap
