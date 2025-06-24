package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/dustin/go-humanize"
	"github.com/itchyny/gojq"
	"golang.design/x/clipboard"
)

//go:embed version
var versionData string

// Styles for the TUI
var (
	lineStyle = lipgloss.NewStyle().
			Padding(0, 1)

	invalidLineStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#666666")).
				Padding(0, 1)

	selectedLineStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("#004499")).
				Foreground(lipgloss.Color("#FFFFFF")).
				Padding(0, 1)

	statusStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("#0066CC")).
			Foreground(lipgloss.Color("#FFFFFF")).
			Padding(0, 1)
)

// Messages for file watching
type newLinesMsg []LogLine

type tickMsg time.Time

// Message for lazy loading
type loadMoreLinesMsg struct {
	err error
}

// Message for loading to end
type loadToEndMsg struct {
	newLines   []LogLine
	err        error
	isComplete bool
}

// Message for spinner animation
type spinnerTickMsg struct{}

// Message for operation completion
type operationCompleteMsg struct {
	operation string
}

// Filter represents a JQ filter
type Filter struct {
	Expression string
	Query      *gojq.Query
	Enabled    bool
}

// LogLine represents a single line from the log file
type LogLine struct {
	LineNumber int
	RawLine    string
	JSONData   map[string]interface{}
	IsValid    bool
}

// Model represents the state of our TUI application
type Model struct {
	filename            string
	lines               []LogLine
	filteredLines       []LogLine // Lines after applying filters
	filters             []Filter  // Active JQ filters
	cursor              int
	viewport            int
	height              int
	width               int
	showPretty          bool
	selectedLine        *LogLine
	prettyViewport      int    // Scroll position in pretty print view
	fileSize            int64  // Track file size for change detection
	lastLineNum         int    // Track the last line number for new lines
	filterMode          bool   // Whether we're in filter input mode
	filterInput         string // Current filter input
	filterCursorPos     int    // Cursor position within filter input
	filterManageMode    bool   // Whether we're in filter management mode
	filterCursor        int    // Cursor position in filter management
	filterEditMode      bool   // Whether we're in filter editing mode
	filterEditInput     string // Current filter edit input
	filterEditCursorPos int    // Cursor position within filter edit input
	lineScrollOffset    int    // Horizontal scroll offset for the highlighted line

	// View transformation fields
	viewMode       bool        // Whether we're in view transform input mode
	viewInput      string      // Current view transform input
	viewCursorPos  int         // Cursor position within view transform input
	viewFilter     *gojq.Query // Active view transformation filter
	viewExpression string      // View transformation expression

	// Lazy loading fields
	file                *os.File // File handle for lazy loading
	filePos             int64    // Current file position (bytes read so far)
	isFileFullyLoaded   bool     // Whether we've read the entire file
	loadingMoreLines    bool     // Whether we're currently loading more lines
	estimatedTotalLines int      // Estimated total lines based on file size and average line length

	// Spinner fields
	showSpinner  bool // Whether to show the spinner
	spinnerFrame int  // Current spinner frame

	// Tail mode field
	tailMode             bool // Whether tail mode is enabled (auto-jump to bottom on new lines)
	needsInitialTailJump bool // Whether we need to jump to end after window size is known

	// Help system
	showHelp     bool // Whether to show the help screen
	helpViewport int  // Scroll position in help view
}

// Init initializes the model
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		tickCmd(), // Start the file watching timer
	)
}

// Update handles messages and updates the model
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.height = msg.Height
		m.width = msg.Width

		// If we need to perform initial tail jump, do it now that we have window size
		if m.needsInitialTailJump {
			visibleLines := m.getVisibleLines()
			if len(visibleLines) > 0 {
				m.cursor = len(visibleLines) - 1
				// Adjust viewport to show the last line at the bottom, just above status bar
				if len(visibleLines) > m.height-1 {
					m.viewport = len(visibleLines) - (m.height - 1)
				} else {
					m.viewport = 0
				}
				m.lineScrollOffset = 0
			}
			m.needsInitialTailJump = false
		}

		return m, nil

	case tea.KeyMsg:
		if m.filterEditMode {
			// Handle filter edit mode
			switch msg.String() {
			case "esc":
				m.filterEditMode = false
				m.filterEditInput = ""
				m.filterEditCursorPos = 0
			case "enter":
				if m.filterEditInput != "" {
					// Remember the current line number we're viewing
					var currentLineNumber int
					visibleLines := m.getVisibleLines()
					if m.cursor < len(visibleLines) {
						currentLineNumber = visibleLines[m.cursor].LineNumber
					}

					// Try to parse the new filter expression
					query, err := gojq.Parse(m.filterEditInput)
					if err == nil {
						// Update the filter
						m.filters[m.filterCursor].Expression = m.filterEditInput
						m.filters[m.filterCursor].Query = query
						m.applyFilters()

						// Restore position based on line number
						m.restorePositionAfterFilter(currentLineNumber)
					}
					// If parsing fails, we ignore the edit (could show error in future)
				}
				m.filterEditMode = false
				m.filterEditInput = ""
				m.filterEditCursorPos = 0
			case "left":
				if m.filterEditCursorPos > 0 {
					m.filterEditCursorPos--
				}
			case "right":
				if m.filterEditCursorPos < len(m.filterEditInput) {
					m.filterEditCursorPos++
				}
			case "home", "ctrl+a":
				m.filterEditCursorPos = 0
			case "end", "ctrl+e":
				m.filterEditCursorPos = len(m.filterEditInput)
			case "backspace":
				if m.filterEditCursorPos > 0 {
					// Delete character before cursor
					m.filterEditInput = m.filterEditInput[:m.filterEditCursorPos-1] + m.filterEditInput[m.filterEditCursorPos:]
					m.filterEditCursorPos--
				}
			case "delete", "ctrl+d":
				if m.filterEditCursorPos < len(m.filterEditInput) {
					// Delete character at cursor
					m.filterEditInput = m.filterEditInput[:m.filterEditCursorPos] + m.filterEditInput[m.filterEditCursorPos+1:]
				}
			case "ctrl+w":
				// Delete word before cursor
				if m.filterEditCursorPos > 0 {
					// Find start of current word
					start := m.filterEditCursorPos - 1
					for start > 0 && m.filterEditInput[start] != ' ' {
						start--
					}
					if m.filterEditInput[start] == ' ' {
						start++
					}
					m.filterEditInput = m.filterEditInput[:start] + m.filterEditInput[m.filterEditCursorPos:]
					m.filterEditCursorPos = start
				}
			case "ctrl+k":
				// Delete from cursor to end
				m.filterEditInput = m.filterEditInput[:m.filterEditCursorPos]
			case "ctrl+v":
				// Paste from clipboard
				if clipboardText := getClipboardText(); clipboardText != "" {
					// Insert clipboard text at cursor position
					m.filterEditInput = m.filterEditInput[:m.filterEditCursorPos] + clipboardText + m.filterEditInput[m.filterEditCursorPos:]
					m.filterEditCursorPos += len(clipboardText)
				}
			default:
				// Add character at cursor position
				if len(msg.String()) == 1 && msg.String() >= " " && msg.String() <= "~" {
					char := msg.String()
					m.filterEditInput = m.filterEditInput[:m.filterEditCursorPos] + char + m.filterEditInput[m.filterEditCursorPos:]
					m.filterEditCursorPos++
				}
			}
			return m, nil
		}

		if m.filterMode {
			// Handle filter input mode
			switch msg.String() {
			case "esc":
				m.filterMode = false
				m.filterInput = ""
				m.filterCursorPos = 0
			case "enter":
				if m.filterInput != "" {
					// Remember the current line number we're viewing
					var currentLineNumber int
					visibleLines := m.getVisibleLines()
					if m.cursor < len(visibleLines) {
						currentLineNumber = visibleLines[m.cursor].LineNumber
					}

					if err := m.addFilter(m.filterInput); err == nil {
						m.applyFilters()
						// Restore position based on line number
						m.restorePositionAfterFilter(currentLineNumber)
					}
				}
				m.filterMode = false
				m.filterInput = ""
				m.filterCursorPos = 0
			case "left":
				if m.filterCursorPos > 0 {
					m.filterCursorPos--
				}
			case "right":
				if m.filterCursorPos < len(m.filterInput) {
					m.filterCursorPos++
				}
			case "home", "ctrl+a":
				m.filterCursorPos = 0
			case "end", "ctrl+e":
				m.filterCursorPos = len(m.filterInput)
			case "backspace":
				if m.filterCursorPos > 0 {
					// Delete character before cursor
					m.filterInput = m.filterInput[:m.filterCursorPos-1] + m.filterInput[m.filterCursorPos:]
					m.filterCursorPos--
				}
			case "delete", "ctrl+d":
				if m.filterCursorPos < len(m.filterInput) {
					// Delete character at cursor
					m.filterInput = m.filterInput[:m.filterCursorPos] + m.filterInput[m.filterCursorPos+1:]
				}
			case "ctrl+w":
				// Delete word before cursor
				if m.filterCursorPos > 0 {
					// Find start of current word
					start := m.filterCursorPos - 1
					for start > 0 && m.filterInput[start] != ' ' {
						start--
					}
					if m.filterInput[start] == ' ' {
						start++
					}
					m.filterInput = m.filterInput[:start] + m.filterInput[m.filterCursorPos:]
					m.filterCursorPos = start
				}
			case "ctrl+k":
				// Delete from cursor to end
				m.filterInput = m.filterInput[:m.filterCursorPos]
			case "ctrl+v":
				// Paste from clipboard
				if clipboardText := getClipboardText(); clipboardText != "" {
					// Insert clipboard text at cursor position
					m.filterInput = m.filterInput[:m.filterCursorPos] + clipboardText + m.filterInput[m.filterCursorPos:]
					m.filterCursorPos += len(clipboardText)
				}
			default:
				// Add character at cursor position
				if len(msg.String()) == 1 && msg.String() >= " " && msg.String() <= "~" {
					char := msg.String()
					m.filterInput = m.filterInput[:m.filterCursorPos] + char + m.filterInput[m.filterCursorPos:]
					m.filterCursorPos++
				}
			}
			return m, nil
		}

		if m.filterManageMode {
			// Handle filter management mode
			switch msg.String() {
			case "esc", "F":
				m.filterManageMode = false
				m.filterCursor = 0
			case "up", "k":
				if m.filterCursor > 0 {
					m.filterCursor--
				}
			case "down", "j":
				if m.filterCursor < len(m.filters)-1 {
					m.filterCursor++
				}
			case "enter", " ":
				// Toggle enabled/disabled
				if m.filterCursor < len(m.filters) {
					// Remember the current line number we're viewing
					var currentLineNumber int
					visibleLines := m.getVisibleLines()
					if m.cursor < len(visibleLines) {
						currentLineNumber = visibleLines[m.cursor].LineNumber
					}

					m.filters[m.filterCursor].Enabled = !m.filters[m.filterCursor].Enabled
					m.applyFilters()

					// Restore position based on line number
					m.restorePositionAfterFilter(currentLineNumber)
				}
			case "d", "x":
				// Delete filter
				if m.filterCursor < len(m.filters) {
					// Remember the current line number we're viewing
					var currentLineNumber int
					visibleLines := m.getVisibleLines()
					if m.cursor < len(visibleLines) {
						currentLineNumber = visibleLines[m.cursor].LineNumber
					}

					m.filters = append(m.filters[:m.filterCursor], m.filters[m.filterCursor+1:]...)
					if m.filterCursor >= len(m.filters) && len(m.filters) > 0 {
						m.filterCursor = len(m.filters) - 1
					}
					m.applyFilters()

					// Restore position based on line number
					m.restorePositionAfterFilter(currentLineNumber)
				}
			case "e":
				// Edit filter
				if m.filterCursor < len(m.filters) {
					m.filterEditMode = true
					m.filterEditInput = m.filters[m.filterCursor].Expression
					m.filterEditCursorPos = len(m.filterEditInput)
				}
			}
			return m, nil
		}

		if m.viewMode {
			// Handle view transform input mode
			switch msg.String() {
			case "esc":
				m.viewMode = false
				m.viewInput = ""
				m.viewCursorPos = 0
			case "enter":
				if m.viewInput != "" {
					// Try to compile the view filter
					query, err := gojq.Parse(m.viewInput)
					if err == nil {
						m.viewFilter = query
						m.viewExpression = m.viewInput
					}
					// If compilation fails, we just ignore the filter (could show error in future)
				} else {
					// Empty input clears the view filter
					m.viewFilter = nil
					m.viewExpression = ""
				}
				m.viewMode = false
				m.viewInput = ""
				m.viewCursorPos = 0
			case "left":
				if m.viewCursorPos > 0 {
					m.viewCursorPos--
				}
			case "right":
				if m.viewCursorPos < len(m.viewInput) {
					m.viewCursorPos++
				}
			case "home", "ctrl+a":
				m.viewCursorPos = 0
			case "end", "ctrl+e":
				m.viewCursorPos = len(m.viewInput)
			case "backspace":
				if m.viewCursorPos > 0 {
					// Delete character before cursor
					m.viewInput = m.viewInput[:m.viewCursorPos-1] + m.viewInput[m.viewCursorPos:]
					m.viewCursorPos--
				}
			case "delete", "ctrl+d":
				if m.viewCursorPos < len(m.viewInput) {
					// Delete character at cursor
					m.viewInput = m.viewInput[:m.viewCursorPos] + m.viewInput[m.viewCursorPos+1:]
				}
			case "ctrl+w":
				// Delete word before cursor
				if m.viewCursorPos > 0 {
					// Find start of current word
					start := m.viewCursorPos - 1
					for start > 0 && m.viewInput[start] != ' ' {
						start--
					}
					if m.viewInput[start] == ' ' {
						start++
					}
					m.viewInput = m.viewInput[:start] + m.viewInput[m.viewCursorPos:]
					m.viewCursorPos = start
				}
			case "ctrl+k":
				// Delete from cursor to end
				m.viewInput = m.viewInput[:m.viewCursorPos]
			case "ctrl+v":
				// Paste from clipboard
				if clipboardText := getClipboardText(); clipboardText != "" {
					// Insert clipboard text at cursor position
					m.viewInput = m.viewInput[:m.viewCursorPos] + clipboardText + m.viewInput[m.viewCursorPos:]
					m.viewCursorPos += len(clipboardText)
				}
			default:
				// Add character at cursor position
				if len(msg.String()) == 1 && msg.String() >= " " && msg.String() <= "~" {
					char := msg.String()
					m.viewInput = m.viewInput[:m.viewCursorPos] + char + m.viewInput[m.viewCursorPos:]
					m.viewCursorPos++
				}
			}
			return m, nil
		}

		// Normal mode key handling
		switch msg.String() {
		case "ctrl+c", "q":
			m.cleanup()
			return m, tea.Quit

		case "f":
			if !m.showPretty && !m.filterManageMode && !m.viewMode {
				m.filterMode = true
				m.filterInput = ""
				m.filterCursorPos = 0
			}

		case "F":
			if !m.showPretty && !m.filterMode && !m.viewMode {
				m.filterManageMode = true
				m.filterCursor = 0
			}

		case "v", "V":
			if !m.showPretty && !m.filterMode && !m.filterManageMode {
				m.viewMode = true
				m.viewInput = m.viewExpression // Pre-fill with current expression
				m.viewCursorPos = len(m.viewInput)
			}

		case "t":
			if !m.showPretty && !m.filterMode && !m.filterManageMode && !m.viewMode {
				m.tailMode = !m.tailMode

				// If tail mode is now enabled, load the entire file and jump to the end
				if m.tailMode {
					if !m.isFileFullyLoaded {
						// Start spinner and trigger loading to end
						m.showSpinner = true
						m.spinnerFrame = 0
						return m, tea.Batch(
							spinnerTickCmd(),
							loadToEndCmd(m.filename, m.file, len(m.lines)),
						)
					} else {
						// File already fully loaded, jump immediately
						visibleLines := m.getVisibleLines()
						if len(visibleLines) > 0 {
							m.cursor = len(visibleLines) - 1
							// Adjust viewport to show the last line at the bottom
							if m.cursor >= m.height-1 { // Account for status bar only
								m.viewport = m.cursor - m.height + 2
								if m.viewport < 0 {
									m.viewport = 0
								}
							} else {
								m.viewport = 0
							}
							m.lineScrollOffset = 0
						}
					}
				}
			}

		case "h":
			if !m.showPretty && !m.filterMode && !m.filterManageMode && !m.viewMode {
				m.showHelp = !m.showHelp
			}

		case "up", "k":
			if m.showHelp {
				// Scroll up in help view
				if m.helpViewport > 0 {
					m.helpViewport--
				}
			} else if m.showPretty {
				// Scroll up in pretty print view
				if m.prettyViewport > 0 {
					m.prettyViewport--
				}
			} else {
				// Normal log navigation
				if m.cursor > 0 {
					m.cursor--
					if m.cursor < m.viewport {
						m.viewport = m.cursor
					}
				}
				// Reset horizontal scroll when moving vertically
				m.lineScrollOffset = 0
			}

		case "down", "j":
			if m.showHelp {
				// Scroll down in help view with bounds checking
				maxScroll := m.calculateHelpMaxScroll()
				if m.helpViewport < maxScroll {
					m.helpViewport++
				}
			} else if m.showPretty {
				// Scroll down in pretty print view with bounds checking
				maxScroll := m.calculatePrettyMaxScroll()
				if m.prettyViewport < maxScroll {
					m.prettyViewport++
				}
			} else {
				// Normal log navigation
				visibleLines := m.getVisibleLines()
				if m.cursor < len(visibleLines)-1 {
					m.cursor++
					// Allow cursor to reach the bottom of the screen
					if m.cursor >= m.viewport+m.height-1 { // Account for status bar only
						m.viewport = m.cursor - m.height + 2
					}

					// Check if we need to load more lines (lazy loading)
					// Trigger loading when we're within 100 lines of the end
					loadTriggerThreshold := 100
					if !m.isFileFullyLoaded && !m.loadingMoreLines &&
						len(m.lines)-m.cursor <= loadTriggerThreshold {
						m.loadingMoreLines = true
						return m, tea.Cmd(func() tea.Msg {
							const chunkSize = 500 // Load 500 lines at a time
							err := m.loadMoreLines(chunkSize)
							return loadMoreLinesMsg{err: err}
						})
					}
				}
				// Reset horizontal scroll when moving vertically
				m.lineScrollOffset = 0
			}

		case "left":
			if !m.showPretty {
				// Scroll highlighted line to the left
				if m.lineScrollOffset > 0 {
					m.lineScrollOffset--
				}
			}

		case "right":
			if !m.showPretty {
				// Scroll highlighted line to the right
				visibleLines := m.getVisibleLines()
				if m.cursor < len(visibleLines) {
					line := visibleLines[m.cursor]
					maxWidth := m.width - 3 // Account for cursor + reserved rightmost column
					if len(line.RawLine) > maxWidth {
						maxScroll := len(line.RawLine) - maxWidth
						if m.lineScrollOffset < maxScroll {
							m.lineScrollOffset++
						}
					}
				}
			}

		case "ctrl+left":
			if !m.showPretty {
				// Fast scroll highlighted line to the left
				if m.lineScrollOffset > 0 {
					m.lineScrollOffset -= 5
					if m.lineScrollOffset < 0 {
						m.lineScrollOffset = 0
					}
				}
			}

		case "ctrl+right":
			if !m.showPretty {
				// Fast scroll highlighted line to the right
				visibleLines := m.getVisibleLines()
				if m.cursor < len(visibleLines) {
					line := visibleLines[m.cursor]
					maxWidth := m.width - 3 // Account for cursor + reserved rightmost column
					if len(line.RawLine) > maxWidth {
						maxScroll := len(line.RawLine) - maxWidth
						m.lineScrollOffset += 5
						if m.lineScrollOffset > maxScroll {
							m.lineScrollOffset = maxScroll
						}
					}
				}
			}

		case "pgup", "page_up":
			if m.showHelp {
				// Page up in help view
				pageSize := m.height - 1 // Account for status bar
				if pageSize < 1 {
					pageSize = 1
				}
				m.helpViewport -= pageSize
				if m.helpViewport < 0 {
					m.helpViewport = 0
				}
			} else if m.showPretty {
				// Page up in pretty print view
				pageSize := m.height - 1 // Account for status bar
				if pageSize < 1 {
					pageSize = 1
				}
				m.prettyViewport -= pageSize
				if m.prettyViewport < 0 {
					m.prettyViewport = 0
				}
			} else {
				// Page up in main log view
				visibleLines := m.getVisibleLines()
				if len(visibleLines) > 0 {
					pageSize := m.height - 1 // Account for status bar
					if pageSize < 1 {
						pageSize = 1
					}

					m.cursor -= pageSize
					if m.cursor < 0 {
						m.cursor = 0
					}

					// Adjust viewport to keep cursor visible
					if m.cursor < m.viewport {
						m.viewport = m.cursor
					}
				}
				// Reset horizontal scroll when moving vertically
				m.lineScrollOffset = 0
			}

		case "pgdn", "page_down", "pgdown":
			if m.showHelp {
				// Page down in help view
				pageSize := m.height - 1 // Account for status bar
				if pageSize < 1 {
					pageSize = 1
				}
				maxScroll := m.calculateHelpMaxScroll()
				m.helpViewport += pageSize
				if m.helpViewport > maxScroll {
					m.helpViewport = maxScroll
				}
			} else if m.showPretty {
				// Page down in pretty print view
				pageSize := m.height - 1 // Account for status bar
				if pageSize < 1 {
					pageSize = 1
				}
				maxScroll := m.calculatePrettyMaxScroll()
				m.prettyViewport += pageSize
				if m.prettyViewport > maxScroll {
					m.prettyViewport = maxScroll
				}
			} else {
				// Page down in main log view
				visibleLines := m.getVisibleLines()
				if len(visibleLines) > 0 {
					pageSize := m.height - 1 // Account for status bar
					if pageSize < 1 {
						pageSize = 1
					}

					m.cursor += pageSize
					if m.cursor >= len(visibleLines) {
						m.cursor = len(visibleLines) - 1
					}

					// Adjust viewport to keep cursor visible
					if m.cursor >= m.viewport+m.height-1 { // Account for status bar only
						m.viewport = m.cursor - m.height + 2
					}

					// Check if we need to load more lines (lazy loading)
					// Trigger loading when we're within 100 lines of the end
					loadTriggerThreshold := 100
					if !m.isFileFullyLoaded && !m.loadingMoreLines &&
						len(m.lines)-m.cursor <= loadTriggerThreshold {
						m.loadingMoreLines = true
						return m, tea.Cmd(func() tea.Msg {
							const chunkSize = 500 // Load 500 lines at a time
							err := m.loadMoreLines(chunkSize)
							return loadMoreLinesMsg{err: err}
						})
					}
				}
				// Reset horizontal scroll when moving vertically
				m.lineScrollOffset = 0
			}

		case "enter", " ":
			if m.showHelp {
				// Do nothing when help screen is open
			} else if m.showPretty {
				// Close pretty print view
				m.showPretty = false
				m.selectedLine = nil
				m.prettyViewport = 0
			} else {
				visibleLines := m.getVisibleLines()
				if m.cursor < len(visibleLines) {
					// Open pretty print view
					m.selectedLine = &visibleLines[m.cursor]
					m.showPretty = true
					m.prettyViewport = 0 // Reset scroll position
				}
			}

		case "esc":
			if m.showHelp {
				// Close help screen
				m.showHelp = false
			} else if m.showPretty {
				// Close pretty print view
				m.showPretty = false
				m.selectedLine = nil
				m.prettyViewport = 0
			} else {
				// Quit the application
				m.cleanup()
				return m, tea.Quit
			}

		case "home":
			if !m.showPretty {
				// Jump to first line
				m.cursor = 0
				m.viewport = 0
				m.lineScrollOffset = 0
				// No spinner needed for Home since it's instant
			}

		case "end":
			if !m.showPretty {
				// Check if we need to load more data
				if !m.isFileFullyLoaded {
					// Start spinner and trigger loading
					m.showSpinner = true
					m.spinnerFrame = 0
					return m, tea.Batch(
						spinnerTickCmd(),
						loadToEndCmd(m.filename, m.file, len(m.lines)),
					)
				} else {
					// File already fully loaded, jump immediately
					visibleLines := m.getVisibleLines()
					if len(visibleLines) > 0 {
						m.cursor = len(visibleLines) - 1
						// Adjust viewport to show the last line at the bottom
						if m.cursor >= m.height-1 { // Account for status bar only
							m.viewport = m.cursor - m.height + 2
						} else {
							m.viewport = 0
						}
						m.lineScrollOffset = 0
					}
				}
			}
		}

	case tickMsg:
		// Check for new lines in the file
		return m, tea.Batch(
			checkForNewLines(m.filename, m.fileSize, m.lastLineNum),
			tickCmd(), // Always schedule the next tick
		)

	case newLinesMsg:
		// Add new lines to the model
		newLines := []LogLine(msg)
		if len(newLines) > 0 {
			// Update state
			m.lines = append(m.lines, newLines...)
			m.lastLineNum = newLines[len(newLines)-1].LineNumber

			// Apply filters to new lines if filters exist
			if len(m.filters) > 0 {
				m.applyFilters()
			}

			// If tail mode is enabled, jump to the bottom automatically
			// This must happen AFTER filters are applied
			if m.tailMode {
				visibleLines := m.getVisibleLines()
				if len(visibleLines) > 0 {
					m.cursor = len(visibleLines) - 1
					// Adjust viewport to show the last line at the bottom
					if m.cursor >= m.height-1 { // Account for status bar only
						m.viewport = m.cursor - m.height + 2
						if m.viewport < 0 {
							m.viewport = 0
						}
					} else {
						m.viewport = 0
					}
					m.lineScrollOffset = 0
				}
			}

			// Update file size (we need to get current file size)
			if stat, err := os.Stat(m.filename); err == nil {
				m.fileSize = stat.Size()
			}
		}
		// Don't schedule another tick here - tickMsg handler does it
		return m, nil

	case loadMoreLinesMsg:
		m.loadingMoreLines = false
		if msg.err != nil {
			// Could show error to user if needed
			// For now, silently fail and stop trying to load more
			m.isFileFullyLoaded = true
		} else {
			// Apply filters to the new lines
			if len(m.filters) > 0 {
				m.applyFilters()
			}
		}
		return m, nil

	case spinnerTickMsg:
		if m.showSpinner {
			m.spinnerFrame++
			return m, spinnerTickCmd() // Continue spinner animation
		}
		return m, nil

	case operationCompleteMsg:
		m.showSpinner = false
		m.spinnerFrame = 0

		if msg.operation == "end" {
			// Jump to last line after loading is complete
			visibleLines := m.getVisibleLines()
			if len(visibleLines) > 0 {
				m.cursor = len(visibleLines) - 1
				// Adjust viewport to show the last line at the bottom
				if m.cursor >= m.height-1 { // Account for status bar only
					m.viewport = m.cursor - m.height + 2
				} else {
					m.viewport = 0
				}
				m.lineScrollOffset = 0
			}
		}
		return m, nil

	case loadToEndMsg:
		// Add new lines from the chunk
		if len(msg.newLines) > 0 {
			m.lines = append(m.lines, msg.newLines...)
			m.lastLineNum = msg.newLines[len(msg.newLines)-1].LineNumber
		}

		if msg.err != nil || msg.isComplete {
			// Loading complete (either error or end of file)
			m.isFileFullyLoaded = true
			m.showSpinner = false
			m.spinnerFrame = 0

			// Close file handle if we're done
			if m.file != nil {
				m.file.Close()
				m.file = nil
			}

			// Apply filters to newly loaded lines
			if len(m.filters) > 0 {
				m.applyFilters()
			}

			// Jump to last line
			visibleLines := m.getVisibleLines()
			if len(visibleLines) > 0 {
				m.cursor = len(visibleLines) - 1
				// Adjust viewport to show the last line at the bottom
				if m.cursor >= m.height-1 { // Account for status bar only
					m.viewport = m.cursor - m.height + 2
				} else {
					m.viewport = 0
				}
				m.lineScrollOffset = 0
			}

			return m, nil
		} else {
			// Continue loading more chunks
			return m, loadToEndCmd(m.filename, m.file, len(m.lines))
		}
	}

	return m, nil
}

// View renders the TUI
func (m Model) View() string {
	if m.showHelp {
		return m.renderHelpView()
	}

	if m.showPretty && m.selectedLine != nil {
		return m.renderPrettyView()
	}

	if m.filterManageMode {
		return m.renderFilterManageView()
	}

	var s strings.Builder

	// Calculate available space for log lines
	// Only status bar takes 1 line at the bottom
	statusLines := 1
	visibleLines := m.height - statusLines
	if visibleLines < 1 {
		visibleLines = 1
	}

	// Get the lines to display (filtered or all)
	displayLines := m.getVisibleLines()

	// Check if all lines are filtered out
	if len(displayLines) == 0 && len(m.lines) > 0 {
		// All lines filtered out
		s.WriteString("All lines filtered out by active filters\n")
		for i := 1; i < visibleLines; i++ {
			s.WriteString("\n")
		}
	} else {
		start := m.viewport
		end := start + visibleLines
		if end > len(displayLines) {
			end = len(displayLines)
		}

		// Display log lines
		linesDisplayed := 0
		for i := start; i < end; i++ {
			line := displayLines[i]
			style := lineStyle
			cursor := "  "

			// Choose style based on line validity and selection
			if i == m.cursor {
				style = selectedLineStyle
				cursor = "> "
			} else if !line.IsValid {
				style = invalidLineStyle
			}

			// Truncate line if too long, accounting for horizontal scroll
			displayLine := line.RawLine

			// Apply view transformation if active
			if m.viewFilter != nil && line.IsValid {
				if transformedData := m.applyViewTransform(line.JSONData); transformedData != "" {
					displayLine = transformedData
				}
				// If transformation fails or returns empty, displayLine remains as line.RawLine
			}

			maxWidth := m.width - 3 // Account for cursor + reserved rightmost column

			// Apply horizontal scrolling for the selected line
			if i == m.cursor && m.lineScrollOffset > 0 && len(displayLine) > m.lineScrollOffset {
				displayLine = displayLine[m.lineScrollOffset:]
			}

			if m.width > 15 && len(displayLine) > maxWidth && maxWidth > 3 {
				displayLine = displayLine[:maxWidth-3] + "..."
			}

			lineText := fmt.Sprintf("%s%s", cursor, displayLine)
			if !line.IsValid {
				lineText += " [INVALID JSON]"
			}

			s.WriteString(style.Render(lineText))
			s.WriteString("\n")
			linesDisplayed++
		}

		// Fill remaining space with empty lines to push status bar to bottom
		for linesDisplayed < visibleLines {
			s.WriteString("\n")
			linesDisplayed++
		}
	}

	// Status bar (pinned to bottom)
	var status string
	if m.filterMode {
		// Create the complete filter bar content
		filterPrefix := "Filter: "
		completeContent := filterPrefix + m.filterInput

		// Calculate padding needed to fill the entire width (reserve rightmost column)
		contentLen := len(completeContent)
		if contentLen < m.width-1 {
			completeContent += strings.Repeat(" ", m.width-1-contentLen)
		}

		// Apply styling to the complete content with cursor positioning
		var styledContent string
		prefixLen := len(filterPrefix)

		if m.filterCursorPos >= len(m.filterInput) {
			// Cursor at end - style everything normally except add cursor at end
			beforeCursor := completeContent[:prefixLen+len(m.filterInput)]
			afterCursor := completeContent[prefixLen+len(m.filterInput):]

			normalStyle := lipgloss.NewStyle().
				Background(lipgloss.Color("#FFD700")).
				Foreground(lipgloss.Color("#000000"))
			cursorStyle := lipgloss.NewStyle().
				Background(lipgloss.Color("#000000")).
				Foreground(lipgloss.Color("#FFD700"))

			if len(afterCursor) > 0 {
				styledContent = normalStyle.Render(beforeCursor) + cursorStyle.Render(string(afterCursor[0])) + normalStyle.Render(afterCursor[1:])
			} else {
				styledContent = normalStyle.Render(beforeCursor) + cursorStyle.Render(" ")
			}
		} else {
			// Cursor in middle - style with cursor at specific position
			beforeCursor := completeContent[:prefixLen+m.filterCursorPos]
			cursorChar := string(completeContent[prefixLen+m.filterCursorPos])
			afterCursor := completeContent[prefixLen+m.filterCursorPos+1:]

			normalStyle := lipgloss.NewStyle().
				Background(lipgloss.Color("#FFD700")).
				Foreground(lipgloss.Color("#000000"))
			cursorStyle := lipgloss.NewStyle().
				Background(lipgloss.Color("#000000")).
				Foreground(lipgloss.Color("#FFD700"))

			styledContent = normalStyle.Render(beforeCursor) + cursorStyle.Render(cursorChar) + normalStyle.Render(afterCursor)
		}

		status = styledContent
	} else if m.viewMode {
		// Create the complete view transform bar content
		viewPrefix := "View: "
		completeContent := viewPrefix + m.viewInput

		// Calculate padding needed to fill the entire width (reserve rightmost column)
		contentLen := len(completeContent)
		if contentLen < m.width-1 {
			completeContent += strings.Repeat(" ", m.width-1-contentLen)
		}

		// Apply styling to the complete content with cursor positioning
		var styledContent string
		prefixLen := len(viewPrefix)

		if m.viewCursorPos >= len(m.viewInput) {
			// Cursor at end - style everything normally except add cursor at end
			beforeCursor := completeContent[:prefixLen+len(m.viewInput)]
			afterCursor := completeContent[prefixLen+len(m.viewInput):]

			normalStyle := lipgloss.NewStyle().
				Background(lipgloss.Color("#9966CC")).
				Foreground(lipgloss.Color("#FFFFFF"))
			cursorStyle := lipgloss.NewStyle().
				Background(lipgloss.Color("#FFFFFF")).
				Foreground(lipgloss.Color("#9966CC"))

			if len(afterCursor) > 0 {
				styledContent = normalStyle.Render(beforeCursor) + cursorStyle.Render(string(afterCursor[0])) + normalStyle.Render(afterCursor[1:])
			} else {
				styledContent = normalStyle.Render(beforeCursor) + cursorStyle.Render(" ")
			}
		} else {
			// Cursor in middle - style with cursor at specific position
			beforeCursor := completeContent[:prefixLen+m.viewCursorPos]
			cursorChar := string(completeContent[prefixLen+m.viewCursorPos])
			afterCursor := completeContent[prefixLen+m.viewCursorPos+1:]

			normalStyle := lipgloss.NewStyle().
				Background(lipgloss.Color("#9966CC")).
				Foreground(lipgloss.Color("#FFFFFF"))
			cursorStyle := lipgloss.NewStyle().
				Background(lipgloss.Color("#FFFFFF")).
				Foreground(lipgloss.Color("#9966CC"))

			styledContent = normalStyle.Render(beforeCursor) + cursorStyle.Render(cursorChar) + normalStyle.Render(afterCursor)
		}

		status = styledContent
	} else {
		enabledCount := 0
		for _, filter := range m.filters {
			if filter.Enabled {
				enabledCount++
			}
		}

		controls := "h=Help"

		// Add tail mode status
		if m.tailMode {
			controls += " | T=on"
		} else {
			controls += " | T=off"
		}

		// Determine total count for status
		totalCount := len(displayLines)
		totalIndicator := ""
		if !m.isFileFullyLoaded {
			if m.estimatedTotalLines > len(m.lines) {
				totalIndicator = fmt.Sprintf("~%s", humanize.Comma(int64(m.estimatedTotalLines)))
			} else {
				totalIndicator = fmt.Sprintf("%s+", humanize.Comma(int64(len(m.lines))))
			}
		} else {
			totalIndicator = humanize.Comma(int64(totalCount))
		}

		// Get the actual line number (important when filters are active)
		currentLineNumber := m.cursor + 1 // Default to cursor position
		if len(displayLines) > 0 && m.cursor >= 0 && m.cursor < len(displayLines) {
			currentLineNumber = displayLines[m.cursor].LineNumber
		}

		// Create main status text without spinner
		statusText := fmt.Sprintf(
			"%s | Line %s/%s | %s",
			m.filename, humanize.Comma(int64(currentLineNumber)), totalIndicator, controls,
		)

		// Add spinner to the right edge if active
		if m.showSpinner {
			spinnerText := getSpinnerChar(m.spinnerFrame)
			// Calculate space available for main text (leave room for spinner + 1 space + reserved rightmost column)
			mainTextWidth := m.width - 3
			if mainTextWidth < 0 {
				mainTextWidth = 0
			}

			// Render main text with limited width
			mainStatus := statusStyle.Width(mainTextWidth).Render(statusText)

			// Create spinner with right alignment
			spinnerStyle := lipgloss.NewStyle().
				Width(2).
				Align(lipgloss.Right).
				Background(statusStyle.GetBackground()).
				Foreground(statusStyle.GetForeground())
			spinnerStatus := spinnerStyle.Render(spinnerText + " ")

			// Combine main status and spinner
			status = lipgloss.JoinHorizontal(lipgloss.Top, mainStatus, spinnerStatus)
		} else {
			status = statusStyle.Width(m.width - 1).Render(statusText)
		}
	}
	s.WriteString(status)

	return s.String()
}

// renderPrettyView renders the pretty-printed JSON view
func (m Model) renderPrettyView() string {
	var s strings.Builder

	// Calculate available space for content
	statusLines := 1
	availableLines := m.height - statusLines
	if availableLines < 1 {
		availableLines = 1
	}

	var allLines []string

	if m.selectedLine.IsValid {
		// Pretty print the JSON with syntax highlighting
		prettyJSON, err := json.MarshalIndent(m.selectedLine.JSONData, "", "  ")
		if err != nil {
			allLines = []string{"Error formatting JSON: " + err.Error()}
		} else {
			// Apply syntax highlighting using Chroma
			highlightedJSON, err := highlightJSON(string(prettyJSON))
			if err != nil {
				// Fallback to non-highlighted JSON if highlighting fails
				jsonLines := strings.Split(string(prettyJSON), "\n")
				for _, line := range jsonLines {
					wrappedLines := m.wrapLine(line, m.width-2)
					allLines = append(allLines, wrappedLines...)
				}
			} else {
				// Split the highlighted JSON into lines and wrap long lines
				jsonLines := strings.Split(highlightedJSON, "\n")
				for _, line := range jsonLines {
					wrappedLines := m.wrapLine(line, m.width-2)
					allLines = append(allLines, wrappedLines...)
				}
			}
		}
	} else {
		allLines = append(allLines, "Invalid JSON:")
		// Wrap the raw line as well
		wrappedRaw := m.wrapLine(m.selectedLine.RawLine, m.width-2)
		allLines = append(allLines, wrappedRaw...)
	}

	// Apply scrolling - ensure we don't scroll past the content
	maxScroll := len(allLines) - availableLines
	if maxScroll < 0 {
		maxScroll = 0
	}

	actualViewport := m.prettyViewport
	if actualViewport > maxScroll {
		actualViewport = maxScroll
	}

	// Display the visible portion
	start := actualViewport
	end := start + availableLines
	if end > len(allLines) {
		end = len(allLines)
	}

	contentLines := 0
	for i := start; i < end; i++ {
		s.WriteString(allLines[i])
		s.WriteString("\n")
		contentLines++
	}

	// Fill remaining space to push status bar to bottom
	for contentLines < availableLines {
		s.WriteString("\n")
		contentLines++
	}

	// Status bar (pinned to bottom) with scroll indicator
	scrollInfo := ""
	if len(allLines) > availableLines {
		scrollInfo = fmt.Sprintf(" | %s/%s", humanize.Comma(int64(start+1)), humanize.Comma(int64(len(allLines))))
	}

	statusText := fmt.Sprintf(
		"Pretty Print - Line %s%s | ↑/↓/PgUp/PgDn to scroll | ENTER/SPACE/ESC to return | q to quit",
		humanize.Comma(int64(m.selectedLine.LineNumber)), scrollInfo,
	)
	status := statusStyle.Width(m.width - 1).Render(statusText)
	s.WriteString(status)

	return s.String()
}

// renderHelpView renders the help screen
func (m Model) renderHelpView() string {
	var s strings.Builder

	// Calculate available space
	statusLines := 1
	availableLines := m.height - statusLines
	if availableLines < 1 {
		availableLines = 1
	}

	// Help content
	helpLines := []string{
		"SIFT - Interactive Log Viewer",
		"",
		"NAVIGATION:",
		"  ↑/↓, k/j        Navigate up/down through log lines",
		"  ←/→             Scroll selected line horizontally",
		"  Ctrl+←/→        Fast horizontal scroll (5 characters)",
		"  PgUp/PgDn       Page up/down through logs",
		"  Home            Jump to first line",
		"  End             Jump to last line (loads entire file if needed)",
		"  Space/Enter     Open pretty-print view for selected line",
		"",
		"FILTERING:",
		"  f               Add a new JQ filter",
		"  F               Open Filter Management",
		"    ↑/↓           Navigate between filters",
		"    Space/Enter   Toggle filter on/off",
		"    e             Edit filter expression",
		"    d/x           Delete filter",
		"    F/Esc         Exit management",
		"",
		"VIEW TRANSFORMATIONS:",
		"  v/V             Enter View mode to transform display",
		"                  (use JQ expressions to format output)",
		"",
		"TAIL MODE:",
		"  t               Toggle Tail Mode (auto-jump to bottom on new lines)",
		"                  Shows T=on/T=off in status bar",
		"",
		"OTHER:",
		"  h               Show/hide this help screen",
		"  q/Ctrl+C        Quit application",
		"  Esc             Close help/pretty-print view or quit",
		"",
		"COMMAND LINE:",
		"  -f <filter>     Apply JQ filter on startup",
		"  -V <view>       Apply view transformation on startup",
		"  -t              Start with Tail Mode enabled",
		"",
		"Press 'h' or 'Esc' to close this help screen",
	}

	// Apply viewport scrolling
	startLine := m.helpViewport
	endLine := startLine + availableLines
	if endLine > len(helpLines) {
		endLine = len(helpLines)
	}

	// Render visible help content
	contentLines := 0
	for i := startLine; i < endLine && contentLines < availableLines; i++ {
		s.WriteString(helpLines[i])
		s.WriteString("\n")
		contentLines++
	}

	// Fill remaining space to push status bar to bottom
	for contentLines < availableLines {
		s.WriteString("\n")
		contentLines++
	}

	// Status bar with scroll indicator
	scrollInfo := ""
	if len(helpLines) > availableLines {
		scrollInfo = fmt.Sprintf(" (%d/%d)", m.helpViewport+1, len(helpLines)-availableLines+1)
	}
	statusText := fmt.Sprintf("%s | Help Screen%s | h/ESC=Close", m.filename, scrollInfo)
	status := statusStyle.Width(m.width - 1).Render(statusText)
	s.WriteString(status)

	return s.String()
}

// renderFilterManageView renders the filter management interface
func (m Model) renderFilterManageView() string {
	var s strings.Builder

	// Calculate available space
	statusLines := 1
	availableLines := m.height - statusLines
	if availableLines < 1 {
		availableLines = 1
	}

	contentLines := 0

	if len(m.filters) == 0 {
		s.WriteString("No filters defined.")
		s.WriteString("\n")
		contentLines++
	} else {
		s.WriteString("Filter Management (ENTER/SPACE to toggle, e to edit, d/x to delete, ESC to exit):")
		s.WriteString("\n\n")
		contentLines += 2

		for i, filter := range m.filters {
			prefix := "  "
			style := lineStyle
			status := "[ ]"

			if filter.Enabled {
				status = "[✓]"
			}

			if i == m.filterCursor {
				prefix = "> "
				style = selectedLineStyle
			}

			line := fmt.Sprintf("%s%s %s", prefix, status, filter.Expression)

			// Truncate if too long
			if len(line) > m.width-2 {
				line = line[:m.width-5] + "..."
			}

			s.WriteString(style.Render(line))
			s.WriteString("\n")
			contentLines++

			if contentLines >= availableLines {
				break
			}
		}
	}

	// Fill remaining space
	for contentLines < availableLines {
		s.WriteString("\n")
		contentLines++
	}

	// Status bar
	var status string
	if m.filterEditMode {
		// Create the complete filter edit bar content
		filterEditPrefix := "Edit Filter: "
		completeContent := filterEditPrefix + m.filterEditInput

		// Calculate padding needed to fill the entire width (reserve rightmost column)
		contentLen := len(completeContent)
		if contentLen < m.width-1 {
			completeContent += strings.Repeat(" ", m.width-1-contentLen)
		}

		// Apply styling to the complete content with cursor positioning
		var styledContent string
		prefixLen := len(filterEditPrefix)

		if m.filterEditCursorPos >= len(m.filterEditInput) {
			// Cursor at end - style everything normally except add cursor at end
			beforeCursor := completeContent[:prefixLen+len(m.filterEditInput)]
			afterCursor := completeContent[prefixLen+len(m.filterEditInput):]

			normalStyle := lipgloss.NewStyle().
				Background(lipgloss.Color("#FF6600")).
				Foreground(lipgloss.Color("#FFFFFF"))
			cursorStyle := lipgloss.NewStyle().
				Background(lipgloss.Color("#FFFFFF")).
				Foreground(lipgloss.Color("#FF6600"))

			if len(afterCursor) > 0 {
				styledContent = normalStyle.Render(beforeCursor) + cursorStyle.Render(string(afterCursor[0])) + normalStyle.Render(afterCursor[1:])
			} else {
				styledContent = normalStyle.Render(beforeCursor) + cursorStyle.Render(" ")
			}
		} else {
			// Cursor in middle - style with cursor at specific position
			beforeCursor := completeContent[:prefixLen+m.filterEditCursorPos]
			cursorChar := string(completeContent[prefixLen+m.filterEditCursorPos])
			afterCursor := completeContent[prefixLen+m.filterEditCursorPos+1:]

			normalStyle := lipgloss.NewStyle().
				Background(lipgloss.Color("#FF6600")).
				Foreground(lipgloss.Color("#FFFFFF"))
			cursorStyle := lipgloss.NewStyle().
				Background(lipgloss.Color("#FFFFFF")).
				Foreground(lipgloss.Color("#FF6600"))

			styledContent = normalStyle.Render(beforeCursor) + cursorStyle.Render(cursorChar) + normalStyle.Render(afterCursor)
		}

		status = styledContent
	} else {
		enabledCount := 0
		for _, filter := range m.filters {
			if filter.Enabled {
				enabledCount++
			}
		}

		var statusText string
		if len(m.filters) == 0 {
			statusText = "Filter Management | No filters defined | F/ESC=exit to main view"
		} else {
			statusText = fmt.Sprintf(
				"Filter Management | %d/%d filters enabled | ENTER/SPACE=toggle | e=edit | d/x=delete | F/ESC=exit",
				enabledCount, len(m.filters),
			)
		}

		status = statusStyle.Width(m.width - 1).Render(statusText)
	}
	s.WriteString(status)

	return s.String()
}

// calculatePrettyMaxScroll calculates the maximum scroll position for pretty print view
func (m Model) calculatePrettyMaxScroll() int {
	if !m.showPretty || m.selectedLine == nil {
		return 0
	}

	statusLines := 1
	availableLines := m.height - statusLines
	if availableLines < 1 {
		availableLines = 1
	}

	var allLines []string

	if m.selectedLine.IsValid {
		prettyJSON, err := json.MarshalIndent(m.selectedLine.JSONData, "", "  ")
		if err != nil {
			allLines = []string{"Error formatting JSON: " + err.Error()}
		} else {
			// Apply syntax highlighting using Chroma
			highlightedJSON, err := highlightJSON(string(prettyJSON))
			if err != nil {
				// Fallback to non-highlighted JSON if highlighting fails
				jsonLines := strings.Split(string(prettyJSON), "\n")
				for _, line := range jsonLines {
					wrappedLines := m.wrapLine(line, m.width-2)
					allLines = append(allLines, wrappedLines...)
				}
			} else {
				// Split the highlighted JSON into lines and wrap long lines
				jsonLines := strings.Split(highlightedJSON, "\n")
				for _, line := range jsonLines {
					wrappedLines := m.wrapLine(line, m.width-2)
					allLines = append(allLines, wrappedLines...)
				}
			}
		}
	} else {
		allLines = append(allLines, "Invalid JSON:")
		wrappedRaw := m.wrapLine(m.selectedLine.RawLine, m.width-2)
		allLines = append(allLines, wrappedRaw...)
	}

	maxScroll := len(allLines) - availableLines
	if maxScroll < 0 {
		maxScroll = 0
	}
	return maxScroll
}

// calculateHelpMaxScroll calculates the maximum scroll position for help view
func (m Model) calculateHelpMaxScroll() int {
	if !m.showHelp {
		return 0
	}

	statusLines := 1
	availableLines := m.height - statusLines
	if availableLines < 1 {
		availableLines = 1
	}

	// Help content lines (same as in renderHelpView)
	helpLines := []string{
		"SIFT - Interactive Log Viewer",
		"",
		"NAVIGATION:",
		"  ↑/↓, k/j        Navigate up/down through log lines",
		"  ←/→             Scroll selected line horizontally",
		"  Ctrl+←/→        Fast horizontal scroll (5 characters)",
		"  PgUp/PgDn       Page up/down through logs",
		"  Home            Jump to first line",
		"  End             Jump to last line (loads entire file if needed)",
		"  Space/Enter     Open pretty-print view for selected line",
		"",
		"FILTERING:",
		"  f               Add a new JQ filter",
		"  F               Open Filter Management",
		"    ↑/↓           Navigate between filters",
		"    Space/Enter   Toggle filter on/off",
		"    e             Edit filter expression",
		"    d/x           Delete filter",
		"    F/Esc         Exit management",
		"",
		"VIEW TRANSFORMATIONS:",
		"  v/V             Enter View mode to transform display",
		"                  (use JQ expressions to format output)",
		"",
		"TAIL MODE:",
		"  t               Toggle Tail Mode (auto-jump to bottom on new lines)",
		"                  Shows T=on/T=off in status bar",
		"",
		"OTHER:",
		"  h               Show/hide this help screen",
		"  q/Ctrl+C        Quit application",
		"  Esc             Close help/pretty-print view or quit",
		"",
		"COMMAND LINE:",
		"  -f <filter>     Apply JQ filter on startup",
		"  -V <view>       Apply view transformation on startup",
		"  -t              Start with Tail Mode enabled",
		"",
		"Press 'h' or 'Esc' to close this help screen",
	}

	maxScroll := len(helpLines) - availableLines
	if maxScroll < 0 {
		maxScroll = 0
	}
	return maxScroll
}

// wrapLine wraps a long line to fit within the specified width
func (m Model) wrapLine(line string, width int) []string {
	if width <= 0 {
		return []string{line}
	}

	if len(line) <= width {
		return []string{line}
	}

	var wrapped []string
	for len(line) > width {
		// Find a good break point (prefer spaces, but break anywhere if needed)
		breakPoint := width
		for i := width - 1; i >= width-20 && i > 0; i-- {
			if line[i] == ' ' || line[i] == ',' || line[i] == ':' {
				breakPoint = i + 1
				break
			}
		}

		wrapped = append(wrapped, line[:breakPoint])
		line = line[breakPoint:]
	}

	if len(line) > 0 {
		wrapped = append(wrapped, line)
	}

	return wrapped
}

// highlightJSON applies syntax highlighting to JSON text
func highlightJSON(jsonText string) (string, error) {
	lexer := lexers.Get("json")
	if lexer == nil {
		lexer = lexers.Fallback
	}

	style := styles.Get("friendly")
	if style == nil {
		style = styles.Fallback
	}

	formatter := formatters.Get("terminal256")
	if formatter == nil {
		formatter = formatters.Fallback
	}

	iterator, err := lexer.Tokenise(nil, jsonText)
	if err != nil {
		return "", err
	}

	var buf strings.Builder
	err = formatter.Format(&buf, style, iterator)
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}

// getClipboardText retrieves text from the system clipboard
func getClipboardText() string {
	// Initialize the clipboard (required for the library)
	err := clipboard.Init()
	if err != nil {
		return ""
	}

	// Read text from clipboard
	data := clipboard.Read(clipboard.FmtText)
	if data == nil {
		return ""
	}

	text := string(data)
	// Clean up the output - remove trailing newlines and ensure it's valid text
	text = strings.TrimSpace(text)
	// Only return single-line text for filter input
	if strings.Contains(text, "\n") {
		// If multi-line, just take the first line
		text = strings.Split(text, "\n")[0]
	}
	return text
}

// loadInitialChunk loads the first chunk of lines from the log file
func loadInitialChunk(filename string, chunkSize int) ([]LogLine, *os.File, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, nil, err
	}

	var lines []LogLine
	scanner := bufio.NewScanner(file)
	lineNumber := 1

	for scanner.Scan() && lineNumber <= chunkSize {
		rawLine := scanner.Text()
		logLine := LogLine{
			LineNumber: lineNumber,
			RawLine:    rawLine,
			IsValid:    false,
		}

		// Try to parse as JSON
		var jsonData map[string]interface{}
		if err := json.Unmarshal([]byte(rawLine), &jsonData); err == nil {
			logLine.JSONData = jsonData
			logLine.IsValid = true
		}

		lines = append(lines, logLine)
		lineNumber++
	}

	if err := scanner.Err(); err != nil {
		file.Close()
		return nil, nil, err
	}

	return lines, file, nil
}

// loadMoreLines loads additional lines from the current file position
func (m *Model) loadMoreLines(chunkSize int) error {
	if m.file == nil || m.isFileFullyLoaded {
		return nil
	}

	scanner := bufio.NewScanner(m.file)
	linesLoaded := 0
	nextLineNumber := len(m.lines) + 1

	for scanner.Scan() && linesLoaded < chunkSize {
		rawLine := scanner.Text()
		logLine := LogLine{
			LineNumber: nextLineNumber,
			RawLine:    rawLine,
			IsValid:    false,
		}

		// Try to parse as JSON
		var jsonData map[string]interface{}
		if err := json.Unmarshal([]byte(rawLine), &jsonData); err == nil {
			logLine.JSONData = jsonData
			logLine.IsValid = true
		}

		m.lines = append(m.lines, logLine)
		nextLineNumber++
		linesLoaded++
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	// Check if we've reached the end of the file
	if linesLoaded < chunkSize {
		m.isFileFullyLoaded = true
		m.file.Close()
		m.file = nil
	}

	// Update last line number
	if len(m.lines) > 0 {
		m.lastLineNum = m.lines[len(m.lines)-1].LineNumber
	}

	return nil
}

// estimateTotalLines estimates the total number of lines in the file
func estimateTotalLines(filename string, sampleSize int) (int, error) {
	file, err := os.Open(filename)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return 0, err
	}
	fileSize := stat.Size()

	if fileSize == 0 {
		return 0, nil
	}

	// Read a sample from the beginning
	scanner := bufio.NewScanner(file)
	var totalBytes int64
	lineCount := 0

	for scanner.Scan() && lineCount < sampleSize {
		totalBytes += int64(len(scanner.Bytes()) + 1) // +1 for newline
		lineCount++
	}

	if err := scanner.Err(); err != nil {
		return 0, err
	}

	if lineCount == 0 {
		return 0, nil
	}

	// Estimate average line size and total lines
	avgLineSize := totalBytes / int64(lineCount)
	estimatedLines := int(fileSize / avgLineSize)

	return estimatedLines, nil
}

// tickCmd returns a command that sends a tick message after a delay
func tickCmd() tea.Cmd {
	return tea.Tick(time.Millisecond*200, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// checkForNewLines checks if the file has grown and returns new lines
func checkForNewLines(filename string, currentSize int64, lastLineNum int) tea.Cmd {
	return func() tea.Msg {
		file, err := os.Open(filename)
		if err != nil {
			return nil
		}
		defer file.Close()

		// Check if file has grown
		stat, err := file.Stat()
		if err != nil {
			return nil
		}

		if stat.Size() <= currentSize {
			return nil // No new content
		}

		// Seek to the previous end of file
		_, err = file.Seek(currentSize, io.SeekStart)
		if err != nil {
			return nil
		}

		// Read new lines
		var newLines []LogLine
		scanner := bufio.NewScanner(file)
		lineNumber := lastLineNum + 1

		for scanner.Scan() {
			rawLine := scanner.Text()
			logLine := LogLine{
				LineNumber: lineNumber,
				RawLine:    rawLine,
				IsValid:    false,
			}

			// Try to parse as JSON
			var jsonData map[string]interface{}
			if err := json.Unmarshal([]byte(rawLine), &jsonData); err == nil {
				logLine.JSONData = jsonData
				logLine.IsValid = true
			}

			newLines = append(newLines, logLine)
			lineNumber++
		}

		if len(newLines) > 0 {
			return newLinesMsg(newLines)
		}

		return nil
	}
}

// restorePositionAfterFilter restores the cursor position after applying filters
func (m *Model) restorePositionAfterFilter(targetLineNumber int) {
	visibleLines := m.getVisibleLines()
	if len(visibleLines) == 0 {
		m.cursor = 0
		m.viewport = 0
		m.lineScrollOffset = 0
		return
	}

	// Find the best position to restore to
	bestPosition := 0

	// Look for the exact line number first
	for i, line := range visibleLines {
		if line.LineNumber == targetLineNumber {
			bestPosition = i
			break
		}
		// If we find a line number greater than target, use the previous position
		if line.LineNumber > targetLineNumber {
			break
		}
		// Keep track of the highest line number below target
		bestPosition = i
	}

	m.cursor = bestPosition

	// Adjust viewport to show the cursor
	if m.cursor < m.viewport {
		m.viewport = m.cursor
	} else if m.cursor >= m.viewport+m.height-1 {
		m.viewport = m.cursor - m.height + 2
		if m.viewport < 0 {
			m.viewport = 0
		}
	}

	// Reset horizontal scroll when position changes
	m.lineScrollOffset = 0
}

// getVisibleLines returns the lines that should be displayed (after filtering)
func (m Model) getVisibleLines() []LogLine {
	if len(m.filters) == 0 {
		return m.lines
	}
	return m.filteredLines
}

// addFilter adds a new JQ filter to the model
func (m *Model) addFilter(expression string) error {
	query, err := gojq.Parse(expression)
	if err != nil {
		return err
	}

	filter := Filter{
		Expression: expression,
		Query:      query,
		Enabled:    true, // New filters are enabled by default
	}

	m.filters = append(m.filters, filter)
	return nil
}

// applyFilters applies all filters to the lines and updates filteredLines
func (m *Model) applyFilters() {
	if len(m.filters) == 0 {
		m.filteredLines = m.lines
		return
	}

	m.filteredLines = nil
	for _, line := range m.lines {
		if m.linePassesAllFilters(line) {
			m.filteredLines = append(m.filteredLines, line)
		}
	}
}

// linePassesAllFilters checks if a line passes all active filters
func (m Model) linePassesAllFilters(line LogLine) bool {
	if !line.IsValid {
		return false // Invalid JSON never passes filters
	}

	for _, filter := range m.filters {
		if !filter.Enabled {
			continue // Skip disabled filters
		}
		iter := filter.Query.Run(line.JSONData)
		result, ok := iter.Next()
		if !ok {
			return false // No result means filter failed
		}
		if err, ok := result.(error); ok && err != nil {
			return false // Error means filter failed
		}
		// Check if result is truthy
		if !isTruthy(result) {
			return false
		}
	}
	return true
}

// isTruthy checks if a value is considered truthy in JQ context
func isTruthy(value interface{}) bool {
	if value == nil {
		return false
	}
	switch v := value.(type) {
	case bool:
		return v
	case int:
		return v != 0
	case float64:
		return v != 0
	case string:
		return v != ""
	case []interface{}:
		return len(v) > 0
	case map[string]interface{}:
		return len(v) > 0
	}
	return true
}

// filterFlags is a custom flag type to handle multiple -f arguments
type filterFlags []string

func (f *filterFlags) String() string {
	return strings.Join(*f, ", ")
}

func (f *filterFlags) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func main() {
	var filters filterFlags
	var viewExpression string
	var showVersion bool
	var tailMode bool
	flag.Var(&filters, "f", "JQ filter expression (can be used multiple times)")
	flag.StringVar(&viewExpression, "V", "", "JQ view transformation expression")
	flag.BoolVar(&showVersion, "v", false, "Show version and exit")
	flag.BoolVar(&tailMode, "t", false, "Start with Tail Mode enabled (auto-jump to bottom on new lines)")
	flag.Parse()

	// Handle version flag
	if showVersion {
		version := strings.TrimSpace(versionData)
		fmt.Printf("v%s\n", version)
		return
	}

	args := flag.Args()
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] <log-file>\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	filename := args[0]

	// Check if file exists and get initial file size before any reads
	stat, err := os.Stat(filename)
	if os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: File '%s' does not exist\n", filename)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting file info: %v\n", err)
		os.Exit(1)
	}

	// Constants for lazy loading
	const initialChunkSize = 1000 // Load first 1000 lines
	const sampleSize = 100        // Sample size for estimating total lines

	var lines []LogLine
	var file *os.File
	var isFileFullyLoaded bool

	if tailMode {
		// Load entire file when tail mode is enabled
		allLines, err := loadAllLines(filename)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading file: %v\n", err)
			os.Exit(1)
		}
		lines = allLines
		file = nil // No need to keep file handle when fully loaded
		isFileFullyLoaded = true
	} else {
		// Load initial chunk of lines
		var err error
		lines, file, err = loadInitialChunk(filename, initialChunkSize)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading file: %v\n", err)
			os.Exit(1)
		}
		isFileFullyLoaded = len(lines) < initialChunkSize
	}

	// Estimate total lines in the file (only needed if not fully loaded)
	estimatedTotal := len(lines)
	if !isFileFullyLoaded {
		var err error
		estimatedTotal, err = estimateTotalLines(filename, sampleSize)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error estimating file size: %v\n", err)
			estimatedTotal = len(lines) // Fallback to current line count
		}
	}

	// Determine the last line number
	lastLineNum := 0
	if len(lines) > 0 {
		lastLineNum = lines[len(lines)-1].LineNumber
	}

	// Initialize the model
	m := Model{
		filename:            filename,
		lines:               lines,
		filteredLines:       lines, // Initialize with all lines
		filters:             []Filter{},
		cursor:              0,
		viewport:            0,
		height:              24, // Default height
		width:               80, // Default width
		fileSize:            stat.Size(),
		lastLineNum:         lastLineNum,
		filterMode:          false,
		filterInput:         "",
		filterCursorPos:     0,
		filterManageMode:    false,
		filterCursor:        0,
		file:                file,
		filePos:             0,
		isFileFullyLoaded:   isFileFullyLoaded,
		loadingMoreLines:    false,
		estimatedTotalLines: estimatedTotal,
		showSpinner:         false,
		spinnerFrame:        0,
		tailMode:            tailMode, // Set tail mode from command line flag
	}

	// Add command-line filters
	for _, filterExpr := range filters {
		if err := m.addFilter(filterExpr); err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing filter '%s': %v\n", filterExpr, err)
			os.Exit(1)
		}
	}

	// Apply filters if any were provided
	if len(filters) > 0 {
		m.applyFilters()
	}

	// Apply view transformation if provided
	if viewExpression != "" {
		query, err := gojq.Parse(viewExpression)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing view expression '%s': %v\n", viewExpression, err)
			os.Exit(1)
		}
		m.viewFilter = query
		m.viewExpression = viewExpression
	}

	// If tail mode is enabled, mark that we need to jump to end once window size is known
	if tailMode {
		m.tailMode = true
		m.needsInitialTailJump = true
	}

	// Start the TUI
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Printf("Error running program: %v\n", err)
		os.Exit(1)
	}
}

// cleanup closes any open file handles
func (m *Model) cleanup() {
	if m.file != nil {
		m.file.Close()
		m.file = nil
	}
}

// getSpinnerChar returns the current spinner character
func getSpinnerChar(frame int) string {
	spinnerChars := []string{"⡀", "⡁", "⡂", "⡃", "⡄", "⡅", "⡆", "⡇", "⡈", "⡉", "⡊", "⡋", "⡌", "⡍", "⡎", "⡏", "⡐", "⡑", "⡒", "⡓", "⡔", "⡕", "⡖", "⡗", "⡘", "⡙", "⡚", "⡛", "⡜", "⡝", "⡞", "⡟", "⡠", "⡡", "⡢", "⡣", "⡤", "⡥", "⡦", "⡧", "⡨", "⡩", "⡪", "⡫", "⡬", "⡭", "⡮", "⡯", "⡰", "⡱", "⡲", "⡳", "⡴", "⡵", "⡶", "⡷", "⡸", "⡹", "⡺", "⡻", "⡼", "⡽", "⡾", "⡿", "⢀", "⢁", "⢂", "⢃", "⢄", "⢅", "⢆", "⢇", "⢈", "⢉", "⢊", "⢋", "⢌", "⢍", "⢎", "⢏", "⢐", "⢑", "⢒", "⢓", "⢔", "⢕", "⢖", "⢗", "⢘", "⢙", "⢚", "⢛", "⢜", "⢝", "⢞", "⢟", "⢠", "⢡", "⢢", "⢣", "⢤", "⢥", "⢦", "⢧", "⢨", "⢩", "⢪", "⢫", "⢬", "⢭", "⢮", "⢯", "⢰", "⢱", "⢲", "⢳", "⢴", "⢵", "⢶", "⢷", "⢸", "⢹", "⢺", "⢻", "⢼", "⢽", "⢾", "⢿", "⣀", "⣁", "⣂", "⣃", "⣄", "⣅", "⣆", "⣇", "⣈", "⣉", "⣊", "⣋", "⣌", "⣍", "⣎", "⣏", "⣐", "⣑", "⣒", "⣓", "⣔", "⣕", "⣖", "⣗", "⣘", "⣙", "⣚", "⣛", "⣜", "⣝", "⣞", "⣟", "⣠", "⣡", "⣢", "⣣", "⣤", "⣥", "⣦", "⣧", "⣨", "⣩", "⣪", "⣫", "⣬", "⣭", "⣮", "⣯", "⣰", "⣱", "⣲", "⣳", "⣴", "⣵", "⣶", "⣷", "⣸", "⣹", "⣺", "⣻", "⣼", "⣽", "⣾", "⣿"}
	return spinnerChars[frame%len(spinnerChars)]
}

// spinnerTickCmd returns a command for spinner animation
func spinnerTickCmd() tea.Cmd {
	return tea.Tick(time.Millisecond*150, func(t time.Time) tea.Msg {
		return spinnerTickMsg{}
	})
}

// loadToEndCmd loads all remaining lines from a file in chunks
func loadToEndCmd(filename string, file *os.File, currentLineCount int) tea.Cmd {
	return func() tea.Msg {
		// If file handle is nil, we need to reopen and seek to the correct position
		var f *os.File
		var err error
		shouldClose := false

		if file != nil {
			f = file
		} else {
			// No file handle, open fresh and seek to correct position
			f, err = os.Open(filename)
			if err != nil {
				return loadToEndMsg{err: err, isComplete: true}
			}
			shouldClose = true

			// Skip lines we've already read by scanning through them
			scanner := bufio.NewScanner(f)
			for i := 0; i < currentLineCount && scanner.Scan(); i++ {
				// Skip already read lines
			}
			if err := scanner.Err(); err != nil {
				if shouldClose {
					f.Close()
				}
				return loadToEndMsg{err: err, isComplete: true}
			}
		}

		if shouldClose {
			defer f.Close()
		}

		// Load remaining lines in chunks
		// The file position should already be correct if we have a file handle
		scanner := bufio.NewScanner(f)
		lineNumber := currentLineCount + 1
		var allNewLines []LogLine
		const chunkSize = 1000

		for scanner.Scan() {
			rawLine := scanner.Text()
			logLine := LogLine{
				LineNumber: lineNumber,
				RawLine:    rawLine,
				IsValid:    false,
			}

			// Try to parse as JSON
			var jsonData map[string]interface{}
			if err := json.Unmarshal([]byte(rawLine), &jsonData); err == nil {
				logLine.JSONData = jsonData
				logLine.IsValid = true
			}

			allNewLines = append(allNewLines, logLine)
			lineNumber++

			// Send chunk if we've loaded enough lines
			if len(allNewLines) >= chunkSize {
				return loadToEndMsg{
					newLines:   allNewLines,
					err:        nil,
					isComplete: false,
				}
			}
		}

		if err := scanner.Err(); err != nil {
			return loadToEndMsg{
				newLines:   allNewLines,
				err:        err,
				isComplete: true,
			}
		}

		// Return final chunk
		return loadToEndMsg{
			newLines:   allNewLines,
			err:        nil,
			isComplete: true,
		}
	}
}

// loadAllLines loads all lines from the file (used when tail mode is enabled)
func loadAllLines(filename string) ([]LogLine, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []LogLine
	scanner := bufio.NewScanner(file)
	lineNumber := 1

	for scanner.Scan() {
		rawLine := scanner.Text()
		logLine := LogLine{
			LineNumber: lineNumber,
			RawLine:    rawLine,
			IsValid:    false,
		}

		// Try to parse as JSON
		var jsonData map[string]interface{}
		if err := json.Unmarshal([]byte(rawLine), &jsonData); err == nil {
			logLine.JSONData = jsonData
			logLine.IsValid = true
		}

		lines = append(lines, logLine)
		lineNumber++
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return lines, nil
}

// applyViewTransform applies the view transformation filter to JSON data
func (m Model) applyViewTransform(jsonData map[string]interface{}) string {
	if m.viewFilter == nil {
		return ""
	}

	// Safely run the filter with error handling
	defer func() {
		if r := recover(); r != nil {
			// If panic occurs, return empty string to fall back to original
			return
		}
	}()

	iter := m.viewFilter.Run(jsonData)
	result, ok := iter.Next()
	if !ok {
		return "" // No result, fall back to original
	}

	// Handle errors
	if err, ok := result.(error); ok && err != nil {
		return "" // Error occurred, fall back to original
	}

	// Convert result to string representation
	switch v := result.(type) {
	case string:
		return v
	case nil:
		return "null"
	case bool:
		if v {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", v)
	case float64:
		return fmt.Sprintf("%g", v)
	default:
		// For complex objects, marshal to JSON
		if jsonBytes, err := json.Marshal(v); err == nil {
			return string(jsonBytes)
		}
		// If marshalling fails, use string representation
		return fmt.Sprintf("%v", v)
	}
}
