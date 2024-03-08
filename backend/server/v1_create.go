package server

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/duke-git/lancet/v2/convertor"
	"github.com/google/uuid"
	"github.com/kataras/iris/v12"
	"github.com/pluja/anysub/db"
	"github.com/pluja/anysub/ent"
	"github.com/pluja/anysub/ent/transcription"
	"github.com/pluja/anysub/models"
	"github.com/pluja/anysub/utils"
	"github.com/pluja/anysub/utils/translations"
	"github.com/pluja/anysub/worker"
)

func (s *Server) createTranscription(c iris.Context) {
	language := strings.ToLower(c.URLParamDefault("lang", "auto"))
	device := strings.ToLower(c.URLParamDefault("device", "cpu"))
	modelSize := strings.ToLower(c.URLParamDefault("modelSize", "small"))
	diarize := strings.ToLower(c.URLParamDefault("diarize", "false"))

	// File handling
	c.SetMaxRequestBodySize(20 * iris.GB)

	// single file
	var err error
	_, fileHeader, err := c.FormFile("file")
	if err != nil {
		c.StopWithError(iris.StatusBadRequest, err)
		return
	}

	safeFileName := fmt.Sprintf("%s-%s", uuid.New().String()[:6], utils.SecureFilename(fileHeader.Filename))
	// Upload the file to specific destination.
	dest := filepath.Join("../uploads", safeFileName)
	c.SaveFormFile(fileHeader, dest)

	var diarizeBool bool
	if diarizeBool, err = convertor.ToBool(diarize); err != nil {
		diarizeBool = false
	}

	client := db.Client()

	var tx *ent.Transcription
	tx, err = client.Transcription.Create().
		SetLanguage(language).
		SetDevice(device).
		SetModelSize(modelSize).
		SetDiarize(diarizeBool).
		SetFileName(safeFileName).
		SetStatus(models.TsStatusPending).
		Save(context.Background())

	if err != nil {
		c.StopWithError(iris.StatusInternalServerError, err)
		return
	}

	// Return the service as JSON
	worker.NewTranscriptionChannel <- true
	c.JSON(tx)
}

func (s *Server) createTranslationTask(c iris.Context) {
	// Validate all required parameters at the beginning
	langTo := c.Params().GetString("langTo")
	if langTo == "" {
		c.StopWithError(iris.StatusBadRequest, fmt.Errorf("a language code must be provided"))
		return
	}

	id, err := c.Params().GetInt("id")
	if err != nil {
		c.StopWithError(iris.StatusBadRequest, err)
		return
	}

	// Context with timeout for database operations
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Get the transcription from the database
	transcription, err := db.Client().Transcription.Query().
		Where(transcription.ID(id)).
		WithTranslations(). // Include the "Comments" edge
		Only(context.Background())
	if err != nil {
		c.StopWithError(iris.StatusNotFound, err)
		return
	}

	// Check if a translation for langTo already exists in the transcription
	for _, tl := range transcription.Edges.Translations {
		if tl.TargetLanguage == langTo {
			// Delete this translation, since it will be overwritten with the new one.
			err = db.Client().Translation.DeleteOneID(tl.ID).Exec(ctx)
			if err != nil {
				c.StopWithError(iris.StatusInternalServerError, err)
				return
			}
		}
	}

	// Create a new translation
	tk, err := db.Client().Translation.
		Create().
		SetSourceLanguage(transcription.Language).
		SetTargetLanguage(langTo).
		SetStatus(models.TsStatusPending).
		SetResult(transcription.Result).
		Save(ctx)
	if err != nil {
		c.StopWithError(iris.StatusInternalServerError, err)
		return
	}

	// Add the translation to the transcription as edge
	_, err = db.Client().Transcription.
		UpdateOneID(transcription.ID).
		AddTranslations(tk).
		Save(ctx)
	if err != nil {
		c.StopWithError(iris.StatusInternalServerError, err)
		return
	}

	// Issue a translation request to the translation service
	go translations.Translate(tk, transcription.ID)

	c.JSON(iris.Map{"ID": transcription.ID, "status": tk.Status})
}
