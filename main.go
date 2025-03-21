package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"
	"gopkg.in/gomail.v2"
	"gopkg.in/yaml.v3"
)

// Конфигурация приложения
type Config struct {
	FTP struct {
		Server   string `yaml:"server"`
		User     string `yaml:"user"`
		Password string `yaml:"password"`
		Dir      string `yaml:"dir"`
		Pattern  string `yaml:"pattern"`
		Period   int    `yaml:"period"`
	} `yaml:"ftp"`

	SMTP struct {
		Host     string   `yaml:"host"`
		Port     string   `yaml:"port"`
		From     string   `yaml:"from"`
		Password string   `yaml:"password"`
		To       []string `yaml:"to"`
		Subject  string   `yaml:"subject"`
		Text     string   `yaml:"text"`
	} `yaml:"smtp"`
}

type ReleaseData struct {
	TargetFolder         string    `json:"TargetFolder"`
	TargetFile           string    `json:"TargetFile"`
	ZipFileName          string    `json:"ZipFileName"`
	Hash                 string    `json:"Hash"`
	Platform             string    `json:"Platform"`
	Major                int       `json:"Major"`
	Minor                int       `json:"Minor"`
	Patch                int       `json:"Patch"`
	Build                int       `json:"Build"`
	TeamcityBuildCounter int       `json:"TeamcityBuildCounter"`
	Tag                  string    `json:"Tag"`
	Sha                  string    `json:"Sha"`
	ShortSha             string    `json:"ShortSha"`
	BranchName           string    `json:"BranchName"`
	When                 time.Time `json:"When"`
	Version              string    `json:"Version"`
	FullVersion          string    `json:"FullVersion"`
}

var config Config

const sentFilesLog = "sent_files.log"

func main() {
	// Загрузка конфигурации
	loadConfig("config.yaml")
	var t time.Duration
	t = time.Duration(config.FTP.Period) * time.Minute
	// Периодичность выполнения
	ticker := time.NewTicker(t)
	defer ticker.Stop()

	for range ticker.C {
		log.Println("Starting FTP file check...")
		files, err := getNewFilesFromFTP()
		if err != nil {
			log.Printf("Error fetching new files: %v\n", err)
			continue
		}

		if len(files) == 0 {
			log.Println("No new files to send.")
			continue
		}

		// Группировка файлов по дате модификации
		groupedFiles := groupFilesByDate(files)

		for date, fileGroup := range groupedFiles {
			// Обработка JSON-файлов
			data, err := processJSONFiles(fileGroup)
			if err != nil {
				log.Printf("Error processing JSON files for date %s: %v\n", date, err)
				continue
			}

			// Отправка письма
			err = sendEmailWithJSONData(data, date)
			if err != nil {
				log.Printf("Error sending email for date %s: %v\n", date, err)
			} else {
				log.Printf("Email with data for date %s sent successfully!\n", date)
				markFilesAsSent(fileGroup)
			}
		}
	}
}

// Загрузка конфигурации из YAML-файла
func loadConfig(filename string) {
	file, err := os.ReadFile(filename)
	if err != nil {
		log.Fatalf("Failed to load config file: %v", err)
	}

	err = yaml.Unmarshal(file, &config)
	if err != nil {
		log.Fatalf("Failed to parse config file: %v", err)
	}
}

// Получение новых файлов с FTP-сервера
func getNewFilesFromFTP() ([]ftp.Entry, error) {
	// Подключение к FTP-серверу
	conn, err := ftp.Dial(config.FTP.Server+":21", ftp.DialWithTimeout(5*time.Second))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to FTP server: %w", err)
	}
	defer conn.Quit()

	// Авторизация
	err = conn.Login(config.FTP.User, config.FTP.Password)
	if err != nil {
		return nil, fmt.Errorf("failed to login to FTP server: %w", err)
	}

	// Переход в директорию
	err = conn.ChangeDir(config.FTP.Dir)
	if err != nil {
		return nil, fmt.Errorf("failed to change directory: %w", err)
	}

	// Получение списка файлов
	files, err := conn.List("")
	if err != nil {
		return nil, fmt.Errorf("failed to list files: %w", err)
	}

	// Фильтрация файлов по маске и проверка на отправку
	var filteredFiles []ftp.Entry
	pattern := regexp.MustCompile(strings.ReplaceAll(config.FTP.Pattern, "*", ".*"))
	for _, file := range files {
		if pattern.MatchString(file.Name) && !isFileAlreadySent(*file) {
			log.Printf("Found new file: %s (Modified: %s)", file.Name, file.Time.Format(time.RFC3339))
			filteredFiles = append(filteredFiles, *file)
		}
	}

	return filteredFiles, nil
}

// Группировка файлов по дате модификации
func groupFilesByDate(files []ftp.Entry) map[string][]ftp.Entry {
	groupedFiles := make(map[string][]ftp.Entry)

	for _, file := range files {
		date := extractDateFromFTPFile(file)
		log.Printf("Grouping file %s by date: %s", file.Name, date)
		groupedFiles[date] = append(groupedFiles[date], file)
	}
	return groupedFiles
}

// Извлечение даты модификации файла
func extractDateFromFTPFile(file ftp.Entry) string {
	// Используем время модификации файла
	modTime := file.Time

	// Форматируем дату в формат YYYYMMDD
	return modTime.Format("2006-01-02")
}

// Обработка JSON-файлов
func processJSONFiles(files []ftp.Entry) ([]ReleaseData, error) {
	var allData []ReleaseData

	for _, file := range files {
		// Скачиваем файл
		filePath := filepath.Join(os.TempDir(), file.Name)
		err := downloadFileFromFTP(file.Name, filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to download file %s: %w", file.Name, err)
		}

		// Читаем содержимое файла
		content, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to read file %s: %w", file.Name, err)
		}

		// Парсим JSON как массив структур
		var jsonData []ReleaseData
		err = json.Unmarshal(content, &jsonData)
		if err != nil {
			return nil, fmt.Errorf("failed to parse JSON from file %s: %w", file.Name, err)
		}

		// Добавляем данные из текущего файла в общий массив
		allData = append(allData, jsonData...)
	}

	return allData, nil
}

// Скачивание файла с FTP
func downloadFileFromFTP(remotePath, localPath string) error {
	conn, err := ftp.Dial(config.FTP.Server+":21", ftp.DialWithTimeout(30*time.Second))
	if err != nil {
		return fmt.Errorf("failed to connect to FTP server: %w", err)
	}
	defer conn.Quit()

	err = conn.Login(config.FTP.User, config.FTP.Password)
	if err != nil {
		return fmt.Errorf("failed to login to FTP server: %w", err)
	}

	err = conn.ChangeDir(config.FTP.Dir)
	if err != nil {
		return fmt.Errorf("failed to change directory: %w", err)
	}

	file, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("failed to create local file: %w", err)
	}
	defer file.Close()

	reader, err := conn.Retr(remotePath)
	if err != nil {
		return fmt.Errorf("failed to retrieve file: %w", err)
	}
	defer reader.Close()

	_, err = file.ReadFrom(reader)
	if err != nil {
		return fmt.Errorf("failed to write file content: %w", err)
	}

	return nil
}

// Отправка письма с данными из JSON
func sendEmailWithJSONData(data []ReleaseData, date string) error {
	// Создание тела письма
	body := fmt.Sprintf(config.SMTP.Text+" от %s\n", date)
	var miniVersion = 0

	for i, entry := range data {
		var plat string
		switch entry.Platform {
		case "none":
			plat = "Не подразумевается"
		default:
			plat = entry.Platform
		}
		var description string
		switch {
		case strings.Contains(entry.ZipFileName, "info"):
			description = "Информация об изменениях"
		case strings.Contains(entry.ZipFileName, "web"):
			description = "Веб-клиент"
		case strings.Contains(entry.ZipFileName, "any-cpu"):
			description = "Универсальная сборка для win, mac, debian (требуется .net)"
		default:
			description = "Сервисы"
		}

		body += fmt.Sprintf("  Файл %d:\n", i+1)
		body += fmt.Sprintf("  Описание: %s\n", description)
		body += fmt.Sprintf("  Папка файла: %s\n", entry.TargetFolder)
		body += fmt.Sprintf("  Файл: %s\n", entry.TargetFile)
		body += fmt.Sprintf("  Имя архива: %s\n", entry.ZipFileName)
		body += fmt.Sprintf("  Платформа: %s\n", plat)
		body += fmt.Sprintf("  Версия: %s\n", entry.Version)
		body += fmt.Sprintf("  Дата: %s\n", entry.When.Format(time.RFC3339))
		body += fmt.Sprintf("  Версия сборки: %d\n", entry.TeamcityBuildCounter)
		body += "\n"
		miniVersion = entry.TeamcityBuildCounter

		// Проверяем, содержит ли TargetFile подстроку "info"
		if strings.Contains(entry.TargetFile, "info") {

			// Скачиваем файл
			localFilePath := filepath.Join(os.TempDir(), filepath.Base(entry.TargetFile))
			err := downloadFileFromFTP(entry.TargetFile, localFilePath)
			if err != nil {
				log.Printf("Failed to download TargetFile %s: %v", entry.TargetFile, err)
				continue
			}

			// Прикрепляем файл к письму
			body += fmt.Sprintf("К письму прикреплен файл измнений: %s\n", entry.TargetFile)
		}
	}

	// Создание нового письма
	m := gomail.NewMessage()
	m.SetHeader("From", config.SMTP.From)
	m.SetHeader("To", config.SMTP.To...)
	m.SetHeader("Subject", fmt.Sprintf("%s - %d  %s", config.SMTP.Subject, miniVersion, date))
	m.SetBody("text/plain", body)

	// Добавляем вложения
	for _, entry := range data {
		if strings.Contains(entry.TargetFile, "info") {
			// Скачиваем файл
			localFilePath := filepath.Join(os.TempDir(), filepath.Base(entry.TargetFile))
			err := downloadFileFromFTP(entry.TargetFile, localFilePath)
			if err != nil {
				log.Printf("Failed to download TargetFile %s: %v", entry.TargetFile, err)
				continue
			}

			// Добавляем файл как вложение
			m.Attach(localFilePath)
		}
	}
	sp, _ := strconv.Atoi(config.SMTP.Port)
	// Настройка SMTP-сервера
	d := gomail.NewDialer(config.SMTP.Host, sp, config.SMTP.From, config.SMTP.Password)
	d.TLSConfig = &tls.Config{InsecureSkipVerify: true} // Отключаем проверку сертификата

	// Отправка письма
	if err := d.DialAndSend(m); err != nil {
		return fmt.Errorf("failed to send email: %w", err)
	}
	return nil
}

// Маркировка файлов как отправленных
func markFilesAsSent(files []ftp.Entry) {
	file, err := os.OpenFile(sentFilesLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Failed to open sent files log: %v\n", err)
		return
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	for _, fileEntry := range files {
		fileRecord := fmt.Sprintf("%s|%s\n", fileEntry.Name, fileEntry.Time.Format("2006-01-02"))
		_, err := writer.WriteString(fileRecord)
		if err != nil {
			log.Printf("Failed to write to sent files log: %v\n", err)
			return
		}
	}
	writer.Flush()
}

// Проверка, был ли файл уже отправлен
func isFileAlreadySent(file ftp.Entry) bool {
	fileRecord := fmt.Sprintf("%s|%s", file.Name, file.Time.Format("2006-01-02"))

	fileLog, err := os.Open(sentFilesLog)
	if err != nil {
		return false
	}
	defer fileLog.Close()

	scanner := bufio.NewScanner(fileLog)
	for scanner.Scan() {
		if scanner.Text() == fileRecord {
			return true
		}
	}
	return false
}
