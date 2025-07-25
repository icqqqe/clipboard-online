package main

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image/png"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/YanxinTang/clipboard-online/utils"
	"github.com/gin-gonic/gin"
	"github.com/lxn/walk"
	"github.com/sirupsen/logrus"
	"golang.org/x/image/bmp"
)

const (
	apiVersion = "1"
)

func setupRoute(engin *gin.Engine) {
	engin.Use(clientName(), logger(), gin.Recovery(), apiVersionChecker(), auth())
	engin.GET("/", getHandler)
	engin.POST("/", setHandler)
	engin.NoRoute(notFoundHandler)
}

func clientName() gin.HandlerFunc {
	return func(c *gin.Context) {
		urlEncodedClientName := c.GetHeader("X-Client-Name")
		clientName, err := url.PathUnescape(urlEncodedClientName)
		if err != nil || clientName == "" {
			clientName = "匿名设备"
		}
		c.Set("clientName", clientName)
		c.Next()
	}
}

func apiVersionChecker() gin.HandlerFunc {
	return func(c *gin.Context) {
		version := c.GetHeader("X-API-Version")
		if version == apiVersion {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "接口版本不匹配，请升级您的捷径",
		})
	}
}

func auth() gin.HandlerFunc {
	return func(c *gin.Context) {

		if app.config.Authkey == "" {
			c.Next()
			return
		}

		reqAuth := c.GetHeader("X-Auth")

		timestamp := time.Now().Unix()
		timeKey := timestamp / app.config.AuthkeyExpiredTimeout

		authCodeRaw := app.config.Authkey + "." + strconv.FormatInt(timeKey, 10)
		authCodeHash := md5.Sum([]byte(authCodeRaw))
		authCodeString := hex.EncodeToString(authCodeHash[:])

		if authCodeString == reqAuth {
			c.Next()
			return
		}

		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error": "操作被拒绝：Authkey 验证失败",
		})
	}
}

func logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		start := time.Now()
		c.Next()
		duration := time.Since(start)
		clientIP := c.ClientIP()
		statusCode := c.Writer.Status()
		clientName := c.GetString("clientName")
		requestLogger := log.WithFields(logrus.Fields{
			"method":     c.Request.Method,
			"statusCode": statusCode,
			"clientIP":   clientIP,
			"path":       path,
			"duration":   duration,
			"clientName": clientName,
		})

		if statusCode >= http.StatusInternalServerError {
			requestLogger.Error()
		} else if statusCode >= http.StatusBadRequest {
			requestLogger.Warn()
		} else {
			requestLogger.Info()
		}
	}
}

type ResponseFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type ResponseFiles []ResponseFile

func getHandler(c *gin.Context) {
	contentType, err := utils.Clipboard().ContentType()
	if err != nil {
		log.WithError(err).Info("failed to get content type of clipboard")
		c.Status(http.StatusBadRequest)
		return
	}

	if contentType == utils.TypeText {
		str, err := walk.Clipboard().Text()
		if err != nil {
			c.Status(http.StatusBadRequest)
			log.WithError(err).Warn("failed to get clipboard")
			return
		}
		log.Info("get clipboard text")
		c.JSON(http.StatusOK, gin.H{
			"type": "text",
			"data": str,
		})
		defer sendCopyNotification(log, c.GetString("clientName"), str)
		return
	}

	if contentType == utils.TypeBitmap {
		bmpBytes, err := utils.Clipboard().Bitmap()
		if err != nil {
			log.WithError(err).Warn("failed to get bmp bytes from clipboard")
		}

		bmpBytesReader := bytes.NewReader(bmpBytes)
		bmpImage, err := bmp.Decode(bmpBytesReader)
		if err != nil {
			log.WithError(err).Warn("failed to decode bmp")
			c.JSON(http.StatusBadRequest, gin.H{"error": "无法获取剪切板内容"})
			return
		}
		pngBytesBuffer := new(bytes.Buffer)
		if err = png.Encode(pngBytesBuffer, bmpImage); err != nil {
			log.WithError(err).Warn("failed to encode bmp as png")
		}

		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无法获取剪切板内容"})
			return
		}

		responseFiles := make([]ResponseFile, 0, 1)
		responseFiles = append(responseFiles, ResponseFile{
			"clipboard.png",
			base64.StdEncoding.EncodeToString(pngBytesBuffer.Bytes()),
		})

		c.JSON(http.StatusOK, gin.H{
			"type": "file",
			"data": responseFiles,
		})
		defer sendCopyNotification(log, c.GetString("clientName"), "[图片媒体] 被复制")
		return
	}

	if contentType == utils.TypeFile {
		// get path of files from clipboard
		filenames, err := utils.Clipboard().Files()
		if err != nil {
			log.WithError(err).Warn("failed to get path of files from clipboard")
			c.Status(http.StatusBadRequest)
			return
		}

		responseFiles := make([]ResponseFile, 0, len(filenames))
		for _, path := range filenames {
			base64, err := readBase64FromFile(path)
			if err != nil {
				log.WithError(err).WithField("filepath", path).Warning("read base64 from file failed")
				continue
			}
			responseFiles = append(responseFiles, ResponseFile{filepath.Base(path), base64})
		}
		log.Info("get clipboard files")

		c.JSON(http.StatusOK, gin.H{
			"type": "file",
			"data": responseFiles,
		})
		defer sendCopyNotification(log, c.GetString("clientName"), "[文件] 被复制")
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": "无法识别剪切板内容"})
}

func readBase64FromFile(path string) (string, error) {
	fileBytes, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(fileBytes), nil
}

// Set clipboard handler

// TextBody is a struct of request body when iOS send files to windows
type TextBody struct {
	Text string `json:"data"`
}

func setHandler(c *gin.Context) {
	if !app.config.ReserveHistory {
		cleanTempFiles()
	}

	contentType := c.GetHeader("X-Content-Type")
	if contentType == utils.TypeText {
		setTextHandler(c)
		return
	}

	setFileHandler(c)
}

func setTextHandler(c *gin.Context) {
	var body TextBody
	if err := c.ShouldBindJSON(&body); err != nil {
		log.WithError(err).Warn("failed to bind text body")
		c.Status(http.StatusBadRequest)
		return
	}

	if err := utils.Clipboard().SetText(body.Text); err != nil {
		log.WithError(err).Warn("failed to set clipboard")
		c.Status(http.StatusBadRequest)
		return
	}

	var notify string = "粘贴内容为空"
	if body.Text != "" {
		notify = body.Text
	}
	defer sendPasteNotification(log, c.GetString("clientName"), notify)
	log.WithField("text", body.Text).Info("set clipboard text")
	c.Status(http.StatusOK)
}

// FileBody is a struct of request body when iOS send files to windows
type FileBody struct {
	Files []File `json:"data"`
}

// File is a struct represtents request file
type File struct {
	Name   string `json:"name"` // filename
	Base64 string `json:"base64"`
	_bytes []byte `json:"-"` // don't use this directly. use *File.Bytes() to get bytes
}

// Bytes returns byte slice of file
func (f *File) Bytes() ([]byte, error) {
	if len(f._bytes) > 0 {
		return f._bytes, nil
	}
	fileBytes, err := base64.StdEncoding.DecodeString(f.Base64)
	if err != nil {
		return []byte{}, nil
	}
	f._bytes = fileBytes
	return fileBytes, nil
}

func setFileHandler(c *gin.Context) {
	contentType := c.GetHeader("X-Content-Type")

	var body FileBody
	if err := c.ShouldBindJSON(&body); err != nil {
		log.WithError(err).Warn("failed to bind file body")
		c.Status(http.StatusBadRequest)
		return
	}

	paths := make([]string, 0, len(body.Files))
	for _, file := range body.Files {
		if file.Name == "-" && file.Base64 == "-" {
			continue
		}
		path := utils.LatestFilename(app.GetTempFilePath(file.Name))
		fileBytes, err := file.Bytes()
		if err != nil {
			log.WithField("filename", file.Name).Warn("failed to read file bytes")
			continue
		}
		if err := newFile(path, fileBytes); err != nil {
			log.WithError(err).WithField("path", path).Warn("failed to create file")
			continue
		}
		paths = append(paths, path)
	}

	if app.config.ReserveHistory {
		// clean paths in _filename.txt
		setLastFilenames(nil)
	} else {
		// write paths to file
		setLastFilenames(paths)
	}

	if err := utils.Clipboard().SetFiles(paths); err != nil {
		log.WithError(err).Warn("failed to set clipboard")
		c.Status(http.StatusBadRequest)
		return
	}

	var notify string
	if contentType == utils.TypeMedia {
		notify = "[图片媒体] 已复制到剪贴板"
	} else {
		notify = "[文件] 已复制到剪贴板"
	}

	defer sendPasteNotification(log, c.GetString("clientName"), notify)
	log.WithField("paths", paths).Info("set clipboard file")
	c.Status(http.StatusOK)
}

func notFoundHandler(c *gin.Context) {
	requestLogger := log.WithFields(logrus.Fields{"user_ip": c.Request.RemoteAddr})
	requestLogger.Info("404 not found")
	c.Status(http.StatusNotFound)
}

func sendCopyNotification(logger *logrus.Logger, client, notify string) {
	if app.config.Notify.Copy {
		sendNotification(logger, "复制", client, notify)
	}
}

func sendPasteNotification(logger *logrus.Logger, client, notify string) {
	if app.config.Notify.Paste {
		sendNotification(logger, "粘贴", client, notify)
	}
}

func sendNotification(logger *logrus.Logger, action, client, notify string) {
	if notify == "" {
		notify = action + "内容为空"
	}
	title := fmt.Sprintf("%s自 %s", action, client)
	if err := app.ni.ShowInfo(title, notify); err != nil {
		logger.WithError(err).WithField("notify", notify).Warn("failed to send notification")
	}
}

func setLastFilenames(filenames []string) {
	path := app.GetTempFilePath("_filename.txt")
	allFilenames := strings.Join(filenames, "\n")
	_ = os.WriteFile(path, []byte(allFilenames), os.ModePerm)
}

func newFile(path string, bytes []byte) error {
	return os.WriteFile(path, bytes, 0644)
}

func cleanTempFiles() {
	path := app.GetTempFilePath("_filename.txt")
	if utils.IsExistFile(path) {
		file, err := os.Open(path)
		if err != nil {
			log.WithError(err).WithField("path", path).Warn("failed to open temp file")
			return
		}
		defer file.Close()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			delPath := scanner.Text()
			if err = os.Remove(delPath); err != nil {
				log.WithError(err).WithField("delPath", delPath).Warn("failed to delete specify path")
			}
		}
	}
}
