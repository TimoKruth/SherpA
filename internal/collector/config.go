package collector

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
	"golang.org/x/sys/unix"
	"sherpa/internal/recoveryarchive"
)

const (
	defaultListenAddr       = ":8080"
	defaultTokenFile        = "/run/secrets/upload-token"
	defaultAgeRecipientFile = "/run/config/age-recipient"
	defaultBorgRepository   = "/run/secrets/borg-repository"
	defaultBorgSSHKeyFile   = "/run/secrets/storage-ssh-key"
	defaultKnownHostsFile   = "/run/config/known_hosts"
	defaultSpoolDir         = "/data/spool"
	defaultStateDir         = "/data/state"
	defaultBorgWorkDir      = "/data/borg"

	defaultMaxCompressedBytes   int64 = 8_589_934_592
	defaultMaxUncompressedBytes int64 = 34_359_738_368
	defaultMaxMembers                 = 100_000
	defaultRetryInterval              = 5 * time.Minute
	defaultPartialMaxAge              = 24 * time.Hour
	defaultStartupGrace               = 26 * time.Hour
	defaultMaxRecoveryAge             = 26 * time.Hour

	defaultBorgBinary        = "borg"
	defaultBorgCreateTimeout = 10 * time.Minute
	defaultBorgQueryTimeout  = 2 * time.Minute

	maxConfigFileBytes int64 = 1 << 20
)

const (
	envListenAddr           = "SHERPA_COLLECTOR_LISTEN_ADDR"
	envTokenFile            = "SHERPA_COLLECTOR_TOKEN_FILE"
	envAgeRecipientFile     = "SHERPA_COLLECTOR_AGE_RECIPIENT_FILE"
	envBorgRepositoryFile   = "SHERPA_COLLECTOR_BORG_REPOSITORY_FILE"
	envBorgSSHKeyFile       = "SHERPA_COLLECTOR_BORG_SSH_KEY_FILE"
	envKnownHostsFile       = "SHERPA_COLLECTOR_KNOWN_HOSTS_FILE"
	envSpoolDir             = "SHERPA_COLLECTOR_SPOOL_DIR"
	envStateDir             = "SHERPA_COLLECTOR_STATE_DIR"
	envBorgWorkDir          = "SHERPA_COLLECTOR_BORG_DIR"
	envMaxCompressedBytes   = "SHERPA_COLLECTOR_MAX_BYTES"
	envMaxUncompressedBytes = "SHERPA_COLLECTOR_MAX_UNCOMPRESSED_BYTES"
	envMaxMembers           = "SHERPA_COLLECTOR_MAX_MEMBERS"
	envRetryInterval        = "SHERPA_COLLECTOR_RETRY_INTERVAL"
	envPartialMaxAge        = "SHERPA_COLLECTOR_PARTIAL_MAX_AGE"
	envStartupGrace         = "SHERPA_COLLECTOR_STARTUP_GRACE"
	envMaxRecoveryAge       = "SHERPA_COLLECTOR_MAX_RECOVERY_AGE"
)

type Config struct {
	ListenAddr     string
	TokenDigest    [32]byte
	AgeRecipient   age.Recipient
	Borg           BorgConfig
	SpoolDir       string
	StateDir       string
	Limits         recoveryarchive.Limits
	RetryInterval  time.Duration
	PartialMaxAge  time.Duration
	StartupGrace   time.Duration
	MaxRecoveryAge time.Duration
}

type BorgConfig struct {
	Binary         string
	Repository     string
	SSHKeyFile     string
	KnownHostsFile string
	WorkDir        string
	CreateTimeout  time.Duration
	QueryTimeout   time.Duration
}

func LoadConfig(getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, errors.New("collector configuration environment is unavailable")
	}

	tokenPath := envOrDefault(getenv, envTokenFile, defaultTokenFile)
	tokenBytes, err := readRequiredFile(tokenPath, envTokenFile, true)
	if err != nil {
		return Config{}, err
	}
	tokenDigest := sha256.Sum256(bytes.TrimSpace(tokenBytes))
	clear(tokenBytes)

	recipientPath := envOrDefault(getenv, envAgeRecipientFile, defaultAgeRecipientFile)
	recipientBytes, err := readRequiredFile(recipientPath, envAgeRecipientFile, false)
	if err != nil {
		return Config{}, err
	}
	recipientText := strings.TrimSpace(string(recipientBytes))
	clear(recipientBytes)
	if strings.HasPrefix(strings.ToUpper(recipientText), "AGE-SECRET-KEY-") {
		return Config{}, settingError(envAgeRecipientFile, "private identity is not allowed")
	}
	recipient, err := age.ParseX25519Recipient(recipientText)
	if err != nil {
		return Config{}, settingError(envAgeRecipientFile, "expected a public X25519 recipient")
	}

	repositoryPath := envOrDefault(getenv, envBorgRepositoryFile, defaultBorgRepository)
	repositoryBytes, err := readRequiredFile(repositoryPath, envBorgRepositoryFile, true)
	if err != nil {
		return Config{}, err
	}
	repository := strings.TrimSpace(string(repositoryBytes))
	clear(repositoryBytes)

	sshKeyPath := envOrDefault(getenv, envBorgSSHKeyFile, defaultBorgSSHKeyFile)
	sshKeyBytes, err := readRequiredFile(sshKeyPath, envBorgSSHKeyFile, true)
	if err != nil {
		return Config{}, err
	}
	clear(sshKeyBytes)

	knownHostsPath := envOrDefault(getenv, envKnownHostsFile, defaultKnownHostsFile)
	knownHostsBytes, err := readRequiredFile(knownHostsPath, envKnownHostsFile, false)
	if err != nil {
		return Config{}, err
	}
	clear(knownHostsBytes)

	limits := recoveryarchive.DefaultLimits()
	limits.MaxCompressedBytes, err = positiveInt64(getenv, envMaxCompressedBytes, defaultMaxCompressedBytes)
	if err != nil {
		return Config{}, err
	}
	limits.MaxUncompressedBytes, err = positiveInt64(getenv, envMaxUncompressedBytes, defaultMaxUncompressedBytes)
	if err != nil {
		return Config{}, err
	}
	limits.MaxMembers, err = positiveInt(getenv, envMaxMembers, defaultMaxMembers)
	if err != nil {
		return Config{}, err
	}

	retryInterval, err := positiveDuration(getenv, envRetryInterval, defaultRetryInterval)
	if err != nil {
		return Config{}, err
	}
	partialMaxAge, err := positiveDuration(getenv, envPartialMaxAge, defaultPartialMaxAge)
	if err != nil {
		return Config{}, err
	}
	startupGrace, err := positiveDuration(getenv, envStartupGrace, defaultStartupGrace)
	if err != nil {
		return Config{}, err
	}
	maxRecoveryAge, err := positiveDuration(getenv, envMaxRecoveryAge, defaultMaxRecoveryAge)
	if err != nil {
		return Config{}, err
	}
	if maxRecoveryAge > startupGrace {
		return Config{}, settingError(envMaxRecoveryAge, "must not exceed startup grace")
	}

	return Config{
		ListenAddr:     envOrDefault(getenv, envListenAddr, defaultListenAddr),
		TokenDigest:    tokenDigest,
		AgeRecipient:   recipient,
		SpoolDir:       envOrDefault(getenv, envSpoolDir, defaultSpoolDir),
		StateDir:       envOrDefault(getenv, envStateDir, defaultStateDir),
		Limits:         limits,
		RetryInterval:  retryInterval,
		PartialMaxAge:  partialMaxAge,
		StartupGrace:   startupGrace,
		MaxRecoveryAge: maxRecoveryAge,
		Borg: BorgConfig{
			Binary:         defaultBorgBinary,
			Repository:     repository,
			SSHKeyFile:     sshKeyPath,
			KnownHostsFile: knownHostsPath,
			WorkDir:        envOrDefault(getenv, envBorgWorkDir, defaultBorgWorkDir),
			CreateTimeout:  defaultBorgCreateTimeout,
			QueryTimeout:   defaultBorgQueryTimeout,
		},
	}, nil
}

func readRequiredFile(path, setting string, secret bool) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, settingError(setting, "file is unavailable or unsafe")
	}
	file := os.NewFile(uintptr(fd), "collector-config")
	if file == nil {
		_ = unix.Close(fd)
		return nil, settingError(setting, "file is unavailable or unsafe")
	}
	defer file.Close()

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, settingError(setting, "file is unavailable or unsafe")
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, settingError(setting, "file must be regular")
	}
	if secret && os.FileMode(stat.Mode).Perm()&0o077 != 0 {
		return nil, settingError(setting, "file permissions are unsafe")
	}

	contents, err := io.ReadAll(io.LimitReader(file, maxConfigFileBytes+1))
	if err != nil || int64(len(contents)) > maxConfigFileBytes {
		clear(contents)
		return nil, settingError(setting, "file cannot be read safely")
	}
	if len(bytes.TrimSpace(contents)) == 0 {
		clear(contents)
		return nil, settingError(setting, "file is empty")
	}
	return contents, nil
}

func positiveInt64(getenv func(string) string, setting string, fallback int64) (int64, error) {
	value := getenv(setting)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, settingError(setting, "must be a positive base-10 integer")
	}
	return parsed, nil
}

func positiveInt(getenv func(string) string, setting string, fallback int) (int, error) {
	value := getenv(setting)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, settingError(setting, "must be a positive base-10 integer")
	}
	return parsed, nil
}

func positiveDuration(getenv func(string) string, setting string, fallback time.Duration) (time.Duration, error) {
	value := getenv(setting)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, settingError(setting, "must be a positive duration")
	}
	return parsed, nil
}

func envOrDefault(getenv func(string) string, setting, fallback string) string {
	if value := getenv(setting); value != "" {
		return value
	}
	return fallback
}

func settingError(setting, classification string) error {
	return fmt.Errorf("%s: %s", setting, classification)
}
