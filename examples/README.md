# Motorsaw Examples

This directory contains example configurations and installation helpers for Motorsaw.

## Files

### Configuration Examples

#### `global-config.yaml`
Complete example of a global configuration file that goes in `~/.motorsaw/config.yaml`.

Includes:
- Two profile examples (`coding_heavy` and `notes_fast`)
- Annotated settings for include/exclude, blacklist/whitelist
- Graph driver configuration with custom prompts
- Security-focused examples

#### `local-config.yaml`
Example of a project-local configuration file (`.config.yaml`).

Use cases:
- Adding vendor directories to search (CLI only)
- Excluding temporary/build directories
- Project-specific file filtering
- Real-world examples for monorepos, Python projects, Go projects

### Skills and Guidelines

#### `SKILLS.md`
Project-specific coding guidelines that Motorsaw can inject into context.

Includes:
- Coding standards (Go style, architecture patterns)
- Security requirements (path handling, SQL safety)
- Testing guidelines (test organization, mocking)
- Code organization (package structure, naming)
- Refactoring guidelines and common pitfalls
- Performance considerations
- Review checklist

**Usage:**
- Place in project root: `./SKILLS.md`
- Or in global skills directory: `~/.motorsaw/skills/SKILLS.md`
- Motorsaw will automatically include this when providing context

### Daemon Setup

#### `motorsawd.service`
Systemd user service unit file for running Motorsaw daemon.

Features:
- Automatic restart on failure
- Reload of the configuration without a restart (`systemctl --user reload motorsawd`, SIGHUP)
- Graceful shutdown (30s timeout for WAL flush)
- Resource limits (2GB RAM, 80% CPU)
- Security hardening (PrivateTmp, ProtectSystem)
- Journal logging

**Manual installation:**
```bash
cp motorsawd.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable motorsawd.service
systemctl --user start motorsawd.service
```

#### `install-daemon.sh`
Automated installation script for the Motorsaw daemon.

**Usage:**
```bash
# Build the daemon first
cd /path/to/motorsaw
go build -o motorsawd ./cmd/motorsawd

# Run installer from examples directory
cd examples
./install-daemon.sh
```

The script will:
1. Copy binary to `~/.local/bin/motorsawd`
2. Install systemd user service
3. Create `~/.motorsaw` directory
4. Provide next steps for configuration

## Quick Start

### 1. Install Dependencies

Make sure you have:
- Go 1.21+ installed
- Ollama running locally
- SQLite with vec0 extension

### 2. Configure Global Settings

```bash
mkdir -p ~/.motorsaw
cp global-config.yaml ~/.motorsaw/config.yaml
# Edit config.yaml to set your include paths and profiles
```

### 3. Install Daemon

```bash
# Build
go build -o motorsawd ./cmd/motorsawd

# Install
cd examples
./install-daemon.sh

# Enable and start
systemctl --user enable motorsawd.service
systemctl --user start motorsawd.service

# Check status
systemctl --user status motorsawd.service
```

### 4. Add Project-Local Config (Optional)

```bash
cd /path/to/your/project
cp /path/to/motorsaw/examples/local-config.yaml ./.config.yaml
# Edit .config.yaml to add project-specific paths
```

### 5. Add Project Skills (Optional)

```bash
cd /path/to/your/project
cp /path/to/motorsaw/examples/SKILLS.md ./SKILLS.md
# Edit SKILLS.md with your project's guidelines
```

## Configuration Tips

### Include/Exclude vs Blacklist/Whitelist

**Two orthogonal filtering systems:**

1. **include/exclude** - Directory-level (indexing scope)
   - `include`: Paths to recursively index
   - `exclude`: Directory patterns to skip
   - Filter: `is_in_include AND NOT is_in_exclude`

2. **blacklist/whitelist** - File-level (regex on absolute paths)
   - `blacklist`: File patterns to reject (applied first)
   - `whitelist`: Exceptions to blacklist
   - Filter: `NOT matches_blacklist OR matches_whitelist`

### Security Best Practices

1. **Use explicit include paths** - Don't include your entire home directory
2. **Blacklist sensitive files** - Use patterns like `.*\.secret$`, `.*\.key$`
3. **Whitelist carefully** - Only add exceptions you truly need
4. **Local configs are additive** - They can only make filtering MORE restrictive

### Performance Tuning

- **chunk_size**: Smaller = more granular, larger = better context (default: 512)
- **overlap**: Prevents splitting related content (default: 64)
- **Resource limits**: Adjust in systemd service based on your system

## Troubleshooting

### Daemon won't start

```bash
# Check logs
journalctl --user -u motorsawd.service -n 50

# Check config syntax
motorsawd --validate-config

# Test manually
motorsawd --debug
```

### Config not found

```bash
# Verify config location
ls -la ~/.motorsaw/config.yaml

# Check config syntax
cat ~/.motorsaw/config.yaml | yq eval '.'
```

### Local config not applying

```bash
# Check config discovery
motorsaw config show  # Will show merged config

# Verify you're in the right directory
pwd
ls .config.yaml
```

## Support

For issues or questions:
- Check DESIGN.md for architecture details
- Review IMPLEMENTATION_SUMMARY.md for technical details
- See .github/copilot-instructions.md for development guidelines
