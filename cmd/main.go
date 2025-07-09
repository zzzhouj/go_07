/*
程序架构分为五大模块：
1. 配置管理：解析命令行参数
2. 检查器模块：可扩展的文件检查策略
3. 工作池模块：并发任务调度
4. 错误处理：独立日志记录
5. 主流程：协调各模块协作
*/

package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// Config 程序配置结构体，封装运行参数
type Config struct {
	Dir        string // 要扫描的目录路径
	MaxWorkers int    // 最大并发worker数量
	ErrorLog   string // 错误日志文件路径
}

// FileError 文件错误记录结构体
type FileError struct {
	Path   string // 文件路径
	Reason string // 错误原因
}

/******************** 文件检查模块 ********************/
// FileChecker 文件检查接口，实现该接口即可创建新的检查规则
type FileChecker interface {
	// Check 检查文件并返回：(是否错误, 错误原因, 系统错误)
	Check(path string) (bool, string, error)
}

// EmptyFileChecker 空文件检查器
type EmptyFileChecker struct{}

// Check 实现空文件检查逻辑
func (c *EmptyFileChecker) Check(path string) (bool, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, "", err // 返回系统错误
	}
	if info.Size() == 0 {
		return true, "empty file", nil // 标记为错误文件
	}
	return false, "", nil
}

// VirusContentChecker 病毒内容检查器
type VirusContentChecker struct{}

// Check 实现病毒内容检查逻辑
func (c *VirusContentChecker) Check(path string) (bool, string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return false, "", err
	}
	if bytes.Contains(content, []byte("virus")) {
		return true, "contains virus", nil
	}
	return false, "", nil
}

/******************** 工作池模块 ********************/
// WorkerPool 工作池管理结构体
type WorkerPool struct {
	wg       sync.WaitGroup // 用于等待所有worker完成
	taskChan chan string    // 文件路径任务通道
	errChan  chan FileError // 错误信息通道
	checkers []FileChecker  // 注册的检查器列表
}

// NewWorkerPool 创建工作池
func NewWorkerPool(maxWorkers int, checkers []FileChecker) *WorkerPool {
	return &WorkerPool{
		taskChan: make(chan string, maxWorkers), // 带缓冲的任务通道
		errChan:  make(chan FileError, 100),     // 缓冲避免阻塞
		checkers: checkers,
	}
}

// Start 启动指定数量的worker协程
func (wp *WorkerPool) Start() {
	for i := 0; i < cap(wp.taskChan); i++ {
		wp.wg.Add(1)
		go wp.worker()
	}
}

// worker 处理任务的核心逻辑
func (wp *WorkerPool) worker() {
	defer wp.wg.Done()
	for path := range wp.taskChan { // 从通道读取待处理文件路径
		var reasons []string

		fmt.Println("checked:", path) // 输出当前处理的文件路径（调试用）
		// 执行所有注册的检查器
		for _, checker := range wp.checkers {
			isError, reason, err := checker.Check(path)
			if err != nil { // 处理系统错误
				wp.errChan <- FileError{Path: path, Reason: err.Error()}
				continue
			}
			if isError {
				reasons = append(reasons, reason) // 收集错误原因
			}
		}

		if len(reasons) > 0 {
			wp.errChan <- FileError{
				Path:   path,
				Reason: fmt.Sprintf("invalid file: %v", reasons),
			}
		}
	}
}

// Stop 安全停止工作池
func (wp *WorkerPool) Stop() {
	close(wp.taskChan) // 关闭任务通道，停止接受新任务
	wp.wg.Wait()       // 等待所有worker结束
	close(wp.errChan)  // 关闭错误通道
}

/******************** 主流程 ********************/
func main() {
	// 1. 初始化配置
	cfg := parseFlags()

	// 2. 注册检查器（可在此处添加新的检查器）
	checkers := []FileChecker{
		&EmptyFileChecker{},
		&VirusContentChecker{},
	}

	// 3. 初始化工作池
	wp := NewWorkerPool(cfg.MaxWorkers, checkers)
	wp.Start()

	// 4. 启动独立的错误处理协程
	var errorHandlerWG sync.WaitGroup
	errorHandlerWG.Add(1)
	go handleErrors(cfg.ErrorLog, wp.errChan, &errorHandlerWG)

	// 5. 遍历目录（注意此时是同步遍历）
	filepath.WalkDir(cfg.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			wp.errChan <- FileError{Path: path, Reason: err.Error()}
			return nil
		}
		if !d.IsDir() {
			wp.taskChan <- path // 将文件路径发送到任务通道
		}
		return nil
	})

	// 6. 清理阶段
	wp.Stop()             // 等待所有任务完成
	errorHandlerWG.Wait() // 等待错误处理完成
}

// parseFlags 解析命令行参数
func parseFlags() Config {
	dir := flag.String("dir", "", "Directory to scan")
	maxWorkers := flag.Int("workers", 5, "Maximum concurrent workers")
	errorLog := flag.String("error-log", "errors.log", "Error log path")
	flag.Parse()

	return Config{
		Dir:        *dir,
		MaxWorkers: *maxWorkers,
		ErrorLog:   *errorLog,
	}
}

// setupLogging 配置日志输出
func setupLogging(cfg Config) {
	// 已废弃，不再使用
}

// handleErrors 错误处理协程
func handleErrors(logPath string, errChan <-chan FileError, wg *sync.WaitGroup) {
	defer wg.Done()

	// 以覆盖模式打开日志文件（每次运行清空旧内容），并设置为log包的输出目标
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		log.Fatal(err)
	}
	defer logFile.Close()
	log.SetOutput(logFile)

	// 持续从错误通道读取错误信息
	for e := range errChan {
		log.Printf("[ERROR] %s: %s", e.Path, e.Reason)
	}
}
