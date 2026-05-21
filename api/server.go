package main

import (
	"log"
	"net/http"

	"job-tracker/db"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

// Job represents the JSON structure for our frontend
type Job struct {
	ID          int    `json:"id"`
	Company     string `json:"company"`
	Role        string `json:"role"`
	Status      string `json:"status"`
	AppliedDate string `json:"applied_date"`
	LastUpdated string `json:"last_updated"`
}

func main() {
	// Connect to our existing SQLite database
	database := db.InitDB("tracker.db")
	defer database.Close()

	// Initialize Gin router
	r := gin.Default()

	// Enable CORS so our frontend can make requests to this API
	r.Use(cors.Default())

	// Endpoint 1: Get all jobs
	r.GET("/api/jobs", func(c *gin.Context) {
		rows, err := database.Query("SELECT id, company, role, status, applied_date, last_updated FROM jobs ORDER BY last_updated DESC")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()

		var jobs []Job
		for rows.Next() {
			var j Job
			if err := rows.Scan(&j.ID, &j.Company, &j.Role, &j.Status, &j.AppliedDate, &j.LastUpdated); err != nil {
				log.Printf("Error scanning row: %v", err)
				continue
			}
			jobs = append(jobs, j)
		}

		// Return an empty array instead of null if there are no jobs
		if jobs == nil {
			jobs = []Job{}
		}
		c.JSON(http.StatusOK, jobs)
	})

	// Endpoint 2: Update job status manually (Step 5.3)
	r.PATCH("/api/jobs/:id", func(c *gin.Context) {
		id := c.Param("id")

		var requestBody struct {
			Status string `json:"status"`
		}

		if err := c.BindJSON(&requestBody); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
			return
		}

		_, err := database.Exec("UPDATE jobs SET status = ?, last_updated = CURRENT_TIMESTAMP WHERE id = ?", requestBody.Status, id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update status"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "Status updated successfully"})
	})

	log.Println("🚀 Server running on http://localhost:8080")
	r.Run(":8080")
}
