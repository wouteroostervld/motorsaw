# Chainsaw Examples

This directory contains example configurations and installation helpers for Chainsaw.

## Files

### Configuration Examples

#### `global-config.yaml`
Complete example of a global configuration file that goes in `~/.chainsaw/config.yaml`.

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
Project-specific coding guidelines that Chainsaw can inject into context.

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
- Or in global skills directory: `~/.chainsaw/skills/SKILLS.md`
- Chainsaw will automatically include this when providing context

### Daemon Setup

#### `chainsawd.service`
Systemd user service unit file for running Chainsaw daemon.

Features:
- Automatic restart on failure
- Reload of the configuration without a restart (`systemctl --user reload chainsawd`, SIGHUP)
- Graceful shutdown (30s timeout for WAL flush)
- Resource limits (2GB RAM, 80% CPU)
- Security hardening (PrivateTmp, ProtectSystem)
- Journal logging

**Manual installation:**
```bash
cp chainsawd.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable chainsawd.service
systemctl --user start chainsawd.service
```

#### `install-daemon.sh`
Automated installation script for the Chainsaw daemon.

**Usage:**
```bash
# Build the daemon first
cd /path/to/chainsaw
go build -o chainsawd ./cmd/chainsawd

# Run installer from examples directory
cd examples
./install-daemon.sh
```

The script will:
1. Copy binary to `~/.local/bin/chainsawd`
2. Install systemd user service
3. Create `~/.chainsaw` directory
4. Provide next steps for configuration

## Quick Start

### 1. Install Dependencies

Make sure you have:
- Go 1.21+ installed
- Ollama running locally
- SQLite with vec0 extension

### 2. Configure Global Settings

```bash
mkdir -p ~/.chainsaw
cp global-config.yaml ~/.chainsaw/config.yaml
# Edit config.yaml to set your include paths and profiles
```

### 3. Install Daemon

```bash
# Build
go build -o chainsawd ./cmd/chainsawd

# Install
cd examples
./install-daemon.sh

# Enable and start
systemctl --user enable chainsawd.service
systemctl --user start chainsawd.service

# Check status
systemctl --user status chainsawd.service
```

### 4. Add Project-Local Config (Optional)

```bash
cd /path/to/your/project
cp /path/to/chainsaw/examples/local-config.yaml ./.config.yaml
# Edit .config.yaml to add project-specific paths
```

### 5. Add Project Skills (Optional)

```bash
cd /path/to/your/project
cp /path/to/chainsaw/examples/SKILLS.md ./SKILLS.md
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
journalctl --user -u chainsawd.service -n 50

# Check config syntax
chainsawd --validate-config

# Test manually
chainsawd --debug
```

### Config not found

```bash
# Verify config location
ls -la ~/.chainsaw/config.yaml

# Check config syntax
cat ~/.chainsaw/config.yaml | yq eval '.'
```

### Local config not applying

```bash
# Check config discovery
chainsaw config show  # Will show merged config

# Verify you're in the right directory
pwd
ls .config.yaml
```

## Support

For issues or questions:
- Check DESIGN.md for architecture details
- Review IMPLEMENTATION_SUMMARY.md for technical details
- See .github/copilot-instructions.md for development guidelines
