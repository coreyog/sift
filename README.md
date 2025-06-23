# Sift

A fast, interactive terminal-based log viewer for JSON-per-line logs with advanced filtering, lazy loading, and real-time tailing capabilities.

This project is 99% written by AI. The rest was written by a human. Some things may be wrong as neither of us truly know what we're doing in a TUI program.

## Features

- **Interactive TUI** - Built with Bubble Tea for a smooth terminal experience
- **JSON-per-line Support** - Automatically parses and validates JSON log entries
- **Lazy Loading** - Efficiently handles large log files by loading data in chunks
- **Real-time Tailing** - Automatically detects and displays new log entries as they're written
- **Advanced Filtering** - Powerful JQ-based filtering with management interface
- **View Transformations** - Transform log display using JQ expressions
- **Pretty Printing** - Syntax-highlighted JSON display with scrolling
- **Horizontal Scrolling** - Navigate long log lines that exceed terminal width
- **Command-line Filters** - Apply filters directly from the command line

## Usage

### Basic Usage

```bash
# View a log file
./sift app.log

# Apply filters from command line
./sift -f 'select(.level == "error")' -f 'select(.service == "api")' app.log

# Apply view transformation from command line
./sift -V '"\(.timestamp) [\(.level)] \(.message)"' app.log

# Combine filters and view transformations
./sift -f 'select(.level | IN("warn"; "error"))' -V '"\(.service): \(.message)"' app.log
```

### Navigation

| Key | Action |
|-----|--------|
| `↑/↓` | Navigate up/down through log lines |
| `←/→` | Scroll selected line horizontally |
| `Ctrl+←/→` | Fast horizontal scroll (5 characters) |
| `PgUp/PgDn` | Page up/down through logs |
| `Home` | Jump to first line |
| `End` | Jump to last line (loads entire file if needed) |
| `t` | Toggle Tail Mode (auto-jump to bottom on new lines) |
| `Space/Enter` | Open pretty-print view for selected line |
| `Esc` | Close pretty-print or quit application |
| `q` | Quit application |

### Filtering

#### Adding Filters
- Press `f` to add a new filter
- Enter a JQ expression (e.g., `select(.level=="error")`)
- Press `Enter` to apply or `Esc` to cancel

#### Managing Filters
- Press `F` to open Filter Management
- Use `↑/↓` to navigate between filters
- Press `Space/Enter` to toggle filter on/off
- Press `e` to edit a filter expression
- Press `d` or `x` to delete a filter
- Press `F` or `Esc` to exit management

#### Filter Examples

```bash
# Show only error logs
select(.level == "error")

# Show logs from specific service
.service == "api"

# Show logs with response time > 1000ms
.response_time > 1000

# Show logs containing specific text
.message | contains("database")

# Complex filter combining conditions
.level == "error" and .service == "payment"

# Filter by timestamp range
.timestamp >= "2023-01-01T00:00:00Z"

# Filter arrays and nested objects
.tags[] == "critical"
.metadata.user_id == 123
```

### View Transformations

- Press `V` to enter View mode
- Enter a JQ expression to transform how lines are displayed
- Press `Enter` to apply or `Esc` to cancel
- Empty input clears the view transformation

#### View Examples

```bash
# Show only message and level
"\(.message) [ \(.level) ]"

# Show timestamp and service
"\(.timestamp) \(.service)"

# Extract specific fields
{time: .timestamp, msg: .message, svc: .service}
```

### Pretty Printing

When viewing individual log entries:
- Press `Space` or `Enter` on any line to open pretty-print view
- Use `↑/↓` or `PgUp/PgDn` to scroll through the formatted JSON
- Syntax highlighting makes JSON structure easy to read
- Long lines are automatically wrapped
- Press `Space`, `Enter`, or `Esc` to return to main view

## Command Line Options

```bash
Usage: sift [options] <log-file>

Options:
  -f string
    	JQ filter expression (can be used multiple times)
  -V string
    	JQ view transformation expression
```

### Examples

```bash
# Apply multiple filters
./sift -f '.level == "error"' -f '.service == "api"' app.log

# Filter by multiple conditions
./sift -f '.level == "error" and .response_time > 1000' app.log

# Filter by date range
./sift -f '.timestamp >= "2023-01-01T00:00:00Z"' app.log

# Apply view transformation
./sift -V '"\(.timestamp) [\(.level)] \(.message)"' app.log

# Combine filters and view transformation
./sift -f '.level == "error"' -V '"\(.service): \(.message)"' app.log
```

## Performance Features

### Lazy Loading

Sift intelligently loads large log files in chunks to provide responsive performance:

- **Initial Load**: Loads first 1,000 lines immediately
- **Progressive Loading**: Automatically loads more lines as you navigate near the end
- **Smart Estimation**: Estimates total file size for accurate progress indication
- **Memory Efficient**: Only keeps necessary data in memory
- **Background Loading**: Non-blocking loading for smooth user experience

### Real-time Tailing

Monitor actively written log files:

- **Automatic Detection**: Detects when files grow and loads new lines
- **Live Updates**: New entries appear automatically without manual refresh
- **Position Preservation**: Maintains your current view position during updates
- **Filter Application**: New lines are automatically filtered using active filters

### Tail Mode

Press `t` to toggle Tail Mode for active log monitoring:

- **Auto-jump to Bottom**: When enabled, automatically jumps to the newest log entries when new lines are detected
- **Full File Loading**: Activating Tail Mode loads the entire file to ensure you see the true end
- **Status Indicator**: Status bar shows `T=on` when active, `T=off` when disabled
- **Smart Behavior**: Only jumps to bottom for new lines that pass active filters
- **Manual Toggle**: Press `t` again to disable and return to normal navigation

**Use Case**: Perfect for monitoring live application logs, error tracking, or following deployment progress in real-time.

## File Format Support

Sift is designed for JSON-per-line log formats where each line contains a valid JSON object:

```json
{"timestamp": "2023-01-01T10:00:00Z", "level": "info", "message": "Server started", "service": "api"}
{"timestamp": "2023-01-01T10:00:01Z", "level": "error", "message": "Database connection failed", "service": "db"}
```

### Invalid Lines

Lines that aren't valid JSON are:
- Still displayed in the log view
- Marked with `[INVALID JSON]` 
- Excluded from filtering (filters only apply to valid JSON)
- Can still be viewed in pretty-print mode (shows raw text)

## Status Bar Information

The status bar shows:
- **Filename**: Currently viewed log file
- **Position**: Current line number and total lines
- **Filter Count**: Number of active filters (when > 1)
- **Tail Mode**: Shows `T=on` when Tail Mode is active, `T=off` when disabled
- **Progress**: Estimated completion for large files
- **Controls**: Available keyboard shortcuts
- **Loading Indicator**: Spinner during background operations

## Tips and Tricks

### Efficient Navigation
- Use `End` to quickly load and jump to the end of large files
- Use `Home` to return to the beginning instantly
- Page Up/Down for rapid navigation through logs

### Filter Management
- Disable filters temporarily instead of deleting them
- Use the edit feature to refine filter expressions
- Combine multiple filters for complex log analysis

### View Transformations
- Use view transformations to focus on relevant data
- Combine with filters for powerful log analysis workflows
- Clear transformations to return to full log lines

### Large File Handling
- Sift handles multi-GB log files efficiently
- Loading indicators show progress for long operations
- File tailing works with actively written logs

## JQ Expression Reference

Sift uses the [gojq](https://github.com/itchyny/gojq) library for filtering and transformations. Common patterns:

### Basic Comparisons
```bash
.level == "error"           # Exact match
.response_time > 1000       # Numeric comparison
.message != null            # Not null check
```

### String Operations
```bash
.message | contains("error")     # String contains
.service | startswith("api")     # String starts with
.level | test("^(error|warn)$")  # Regex match
```

### Array Operations
```bash
.tags[] == "critical"            # Any array element matches
.tags | length > 0               # Array not empty
.errors[0].code == 500          # First array element
```

### Object Navigation
```bash
.request.method == "POST"        # Nested object access
.metadata.user.id == 123        # Deep nesting
has("error_code")               # Key existence check
```

### Logical Operations
```bash
.level == "error" and .service == "api"     # AND
.level == "error" or .level == "warn"       # OR
not(.level == "debug")                      # NOT
```

## Dependencies

- [Bubble Tea](https://github.com/charmbracelet/bubbletea) - TUI framework
- [Lipgloss](https://github.com/charmbracelet/lipgloss) - Styling and layout
- [gojq](https://github.com/itchyny/gojq) - JQ implementation for Go
- [Chroma](https://github.com/alecthomas/chroma) - Syntax highlighting
- [go-humanize](https://github.com/dustin/go-humanize) - Human-readable numbers
- [clipboard](https://golang.design/x/clipboard) - Clipboard integration

## License

[Add your license here]

## Contributing

[Add contributing guidelines here]
