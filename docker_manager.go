package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

type LanguageConfig struct {
	Image         string
	FileExtension string
	FileName      string
	RunCommand    []string
	SetupCommands []string
}

type DockerManager struct {
	client           *client.Client
	languageConfigs  map[string]LanguageConfig
	activeContainers map[string]string
	maxExecutionTime time.Duration
	maxMemory        int64
	maxCPUShares     int64
	mu               sync.RWMutex
	ctx              context.Context
}

type ExecutionResult struct {
	Success       bool
	Output        string
	Stderr        string
	ExitCode      int
	ExecutionTime int64
	MemoryUsage   int64
}

func NewDockerManager() (*DockerManager, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	ctx := context.Background()
	if _, err := cli.Ping(ctx); err != nil {
		return nil, fmt.Errorf("Docker connection failed: %w", err)
	}

	dm := &DockerManager{
		client:           cli,
		activeContainers: make(map[string]string),
		maxExecutionTime: 30 * time.Second,
		maxMemory:        256 * 1024 * 1024,
		maxCPUShares:     512,
		ctx:              ctx,
	}

	dm.initializeLanguageConfigs()

	go dm.pullRequiredImages()

	log.Println("Docker manager initialized successfully")

	return dm, nil
}

func (dm *DockerManager) initializeLanguageConfigs() {
	dm.languageConfigs = map[string]LanguageConfig{
		"javascript": {
			Image:         "node:16-alpine",
			FileExtension: "js",
			FileName:      "main.js",
			RunCommand:    []string{"node", "/app/main.js"},
			SetupCommands: []string{},
		},
		"python": {
			Image:         "python:3.9-alpine",
			FileExtension: "py",
			FileName:      "main.py",
			RunCommand:    []string{"python", "/app/main.py"},
			SetupCommands: []string{},
		},
		"cpp": {
			Image:         "gcc:9-alpine",
			FileExtension: "cpp",
			FileName:      "main.cpp",
			RunCommand:    []string{"sh", "-c", "cd /app && g++ -o main main.cpp && ./main"},
			SetupCommands: []string{},
		},
		"go": {
			Image:         "golang:1.19-alpine",
			FileExtension: "go",
			FileName:      "main.go",
			RunCommand:    []string{"go", "run", "/app/main.go"},
			SetupCommands: []string{},
		},
		"rust": {
			Image:         "rust:1.65-alpine",
			FileExtension: "rs",
			FileName:      "main.rs",
			RunCommand:    []string{"sh", "-c", "cd /app && rustc main.rs && ./main"},
			SetupCommands: []string{},
		},
	}
}

func (dm *DockerManager) pullRequiredImages() {
	images := make(map[string]bool)
	for _, config := range dm.languageConfigs {
		images[config.Image] = true
	}

	for image := range images {
		log.Printf("Pulling Docker image: %s", image)
		reader, err := dm.client.ImagePull(dm.ctx, image, types.ImagePullOptions{})
		if err != nil {
			log.Printf("Failed to pull image %s: %v", image, err)
			continue
		}
		io.Copy(io.Discard, reader)
		reader.Close()
		log.Printf("Successfully pulled: %s", image)
	}
}

func (dm *DockerManager) ExecuteCode(code, language, userID string) (*ExecutionResult, error) {
	config, exists := dm.languageConfigs[language]
	if !exists {
		return nil, fmt.Errorf("Unsupported language: %s", language)
	}

	if err := dm.validateCode(code, language); err != nil {
		return nil, err
	}

	tempDir, err := os.MkdirTemp("", "code-exec-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	codePath := filepath.Join(tempDir, config.FileName)
	if err := os.WriteFile(codePath, []byte(code), 0644); err != nil {
		return nil, fmt.Errorf("failed to write code file: %w", err)
	}

	containerID, err := dm.createContainer(config, tempDir)
	if err != nil {
		return nil, err
	}

	dm.mu.Lock()
	dm.activeContainers[userID] = containerID
	dm.mu.Unlock()

	defer func() {
		dm.mu.Lock()
		delete(dm.activeContainers, userID)
		dm.mu.Unlock()
		dm.removeContainer(containerID)
	}()

	return dm.runContainer(containerID)
}

func (dm *DockerManager) createContainer(config LanguageConfig, tempDir string) (string, error) {
	containerConfig := &container.Config{
		Image:        config.Image,
		Cmd:          config.RunCommand,
		WorkingDir:   "/app",
		AttachStderr: true,
		AttachStdout: true,
		Tty:          false,
		User:         "nobody",
	}

	hostConfig := &container.HostConfig{
		Binds: []string{
			fmt.Sprintf("%s:/app:ro", tempDir),
		},
		Resources: container.Resources{
			Memory:    dm.maxMemory,
			CPUShares: dm.maxCPUShares,
			PidsLimit: func() *int64 { i := int64(50); return &i }(),
		},
		NetworkMode:    "none",
		ReadonlyRootfs: true,
		Tmpfs: map[string]string{
			"/tmp": "rw,noexec,nosuid,size=10m",
		},
		SecurityOpt: []string{
			"no-new-privileges:true",
		},
		CapDrop: []string{"ALL"},
	}

	resp, err := dm.client.ContainerCreate(
		dm.ctx,
		containerConfig,
		hostConfig,
		nil,
		nil,
		"",
	)

	if err != nil {
		return "", fmt.Errorf("failed to create container: %w", err)
	}

	return resp.ID, nil
}

func (dm *DockerManager) runContainer(containerID string) (*ExecutionResult, error) {
	startTime := time.Now()

	if err := dm.client.ContainerStart(dm.ctx, containerID, types.ContainerStartOptions{}); err != nil {
		return nil, fmt.Errorf("failed to start container: %w", err)
	}

	ctx, cancel := context.WithTimeout(dm.ctx, dm.maxExecutionTime)
	defer cancel()

	statusCh, errCh := dm.client.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)

	var exitCode int64
	select {
	case err := <-errCh:
		if err != nil {
			dm.client.ContainerKill(dm.ctx, containerID, "SIGKILL")
			return &ExecutionResult{
				Success:  false,
				Output:   "",
				Stderr:   fmt.Sprintf("Execution error: %v", err),
				ExitCode: 1,
			}, nil
		}
	case status := <-statusCh:
		exitCode = status.StatusCode
	}

	executionTime := time.Since(startTime).Milliseconds()

	stdout, stderr, err := dm.getContainerLogs(containerID)
	if err != nil {
		return nil, err
	}

	stats, err := dm.client.ContainerStats(dm.ctx, containerID, false)
	var memoryUsage int64
	if err == nil {
		defer stats.Body.Close()
		var statsData types.StatsJSON
		if err := json.NewDecoder(stats.Body).Decode(&statsData); err == nil {
			memoryUsage = int64(statsData.MemoryStats.Usage)
		}
	}

	return &ExecutionResult{
		Success:       exitCode == 0,
		Output:        stdout,
		Stderr:        stderr,
		ExitCode:      int(exitCode),
		ExecutionTime: executionTime,
		MemoryUsage:   memoryUsage,
	}, nil
}

func (dm *DockerManager) getContainerLogs(containerID string) (string, string, error) {
	options := types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
	}

	logs, err := dm.client.ContainerLogs(dm.ctx, containerID, options)
	if err != nil {
		return "", "", fmt.Errorf("failed to get logs: %w", err)
	}
	defer logs.Close()

	var stdoutBuf, stderrBuf bytes.Buffer

	header := make([]byte, 8)
	for {
		_, err := logs.Read(header)
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", err
		}

		// Stream type: 0=stdin, 1=stdout, 2=stderr
		streamType := header[0]

		// Size of the frame
		size := int(header[4])<<24 | int(header[5])<<16 | int(header[6])<<8 | int(header[7])

		data := make([]byte, size)
		_, err = io.ReadFull(logs, data)
		if err != nil {
			break
		}

		switch streamType {
		case 1: // stdout
			stdoutBuf.Write(data)
		case 2: // stderr
			stderrBuf.Write(data)
		}
	}

	return stdoutBuf.String(), stderrBuf.String(), nil
}

func (dm *DockerManager) removeContainer(containerID string) {
	removeOptions := types.ContainerRemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	}

	if err := dm.client.ContainerRemove(dm.ctx, containerID, removeOptions); err != nil {
		log.Printf("Failed to remove container %s: %v", containerID, err)
	}
}

func (dm *DockerManager) validateCode(code, language string) error {
	if len(code) > 10000 {
		return fmt.Errorf("code is too long (max 10,000 characters)")
	}

	if len(code) == 0 {
		return fmt.Errorf("code is empty")
	}

	dangerousPatterns := map[string][]string{
		"javascript": {
			`require\s*\(\s*['"]fs['"]`,
			`require\s*\(\s*['"]child_process['"]`,
			`require\s*\(\s*['"]net['"]`,
			`require\s*\(\s*['"]http['"]`,
			`process\.exit`,
			`process\.env`,
		},
		"python": {
			`import\s+os`,
			`import\s+subprocess`,
			`import\s+socket`,
			`from\s+os\s+import`,
			`__import__\s*\(`,
			`exec\s*\(`,
			`eval\s*\(`,
		},
	}

	patterns, exists := dangerousPatterns[language]
	if !exists {
		return nil
	}

	for _, pattern := range patterns {
		matched, err := regexp.MatchString(pattern, code)
		if err != nil {
			continue
		}
		if matched {
			return fmt.Errorf("code contains potentially unsafe operations: %s", pattern)
		}
	}

	return nil
}

func (dm *DockerManager) GetCodeTemplate(language string) string {
	templates := map[string]string{
		"javascript": `// JavaScript Code
console.log("Hello, World!");

// You can use modern ES6+ features
const numbers = [1, 2, 3, 4, 5];
const doubled = numbers.map(n => n * 2);
console.log("Doubled:", doubled);`,

		"python": `# Python Code
print("Hello, World!")

# You can use Python standard library
import math
numbers = [1, 2, 3, 4, 5]
squared = [n**2 for n in numbers]
print("Squared:", squared)`,

		"cpp": `// C++ Code
#include <iostream>
#include <vector>

int main() {
    std::cout << "Hello, World!" << std::endl;
    
    // Simple vector example
    std::vector<int> numbers = {1, 2, 3, 4, 5};
    for (int num : numbers) {
        std::cout << num * 2 << " ";
    }
    std::cout << std::endl;
    
    return 0;
}`,

		"go": `// Go Code
package main

import "fmt"

func main() {
    fmt.Println("Hello, World!")
    
    // Simple slice example
    numbers := []int{1, 2, 3, 4, 5}
    for _, num := range numbers {
        fmt.Print(num*2, " ")
    }
    fmt.Println()
}`,

		"rust": `// Rust Code
fn main() {
    println!("Hello, World!");
    
    // Simple vector example
    let numbers = vec![1, 2, 3, 4, 5];
    let doubled: Vec<i32> = numbers.iter().map(|&x| x * 2).collect();
    println!("Doubled: {:?}", doubled);
}`,
	}

	template, exists := templates[language]
	if !exists {
		return templates["javascript"]
	}
	return template
}

func (dm *DockerManager) Cleanup() {
	log.Println("Cleaning up Docker containers...")

	dm.mu.Lock()
	containerIDs := make([]string, 0, len(dm.activeContainers))
	for _, id := range dm.activeContainers {
		containerIDs = append(containerIDs, id)
	}
	dm.mu.Unlock()

	for _, containerID := range containerIDs {
		dm.removeContainer(containerID)
	}

	dm.cleanupOrphanedContainers()

	log.Println("Docker cleanup completed")
}

func (dm *DockerManager) cleanupOrphanedContainers() {
	containers, err := dm.client.ContainerList(dm.ctx, types.ContainerListOptions{
		All: true,
	})
	if err != nil {
		log.Printf("Failed to list containers: %v", err)
		return
	}

	for _, container := range containers {
		for _, config := range dm.languageConfigs {
			if container.Image == config.Image {
				dm.removeContainer(container.ID)
				break
			}
		}
	}
}

func (dm *DockerManager) GetActiveExecutions() int {
	dm.mu.RLock()
	defer dm.mu.RUnlock()
	return len(dm.activeContainers)
}

func (dm *DockerManager) HealthCheck() error {
	_, err := dm.client.Ping(dm.ctx)
	return err
}
