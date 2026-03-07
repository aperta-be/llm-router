package config

import "os"

const DefaultClassificationPrompt = `You are a task classifier. Reply with only one word: thinking, coding, simple, or general.

Examples:
User: Write a Python script to parse JSON -> coding
User: What is 2+2 -> simple
User: Debug this SQL query -> coding
User: Fix the bug in this function -> coding
User: Plan a product launch roadmap -> thinking
User: Hello -> simple
User: What time is it -> simple
User: Implement a binary search tree -> coding
User: Explain how recursion works in code -> coding
User: Analyze the pros and cons of microservices -> thinking
User: What is the capital of Japan -> simple
User: Refactor this Go function -> coding
User: Write unit tests for this class -> coding
User: Summarize this article -> general
User: Translate this paragraph to French -> general
User: Why is the sky blue -> general
User: Plan a 3-month SaaS roadmap -> thinking
User: Solve this logic puzzle step by step -> thinking
User: Write a short story about a robot -> general

Now classify the following user message with a single word:`

type Config struct {
	OllamaBaseURL        string
	ClassifierModel      string
	ThinkingModel        string
	CodingModel          string
	SimpleModel          string
	DefaultModel         string
	ClassificationPrompt string
	ClassifierTimeoutS   int
	CacheTTLS            int
	CacheMaxSize         int
	Port                 string
	DBPath               string
	AdminUsername        string
	AdminPassword        string
}

func Load() *Config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	ollamaURL := os.Getenv("OLLAMA_BASE_URL")
	if ollamaURL == "" {
		ollamaURL = "http://0.0.0.0:11434"
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "router.db"
	}

	adminUser := os.Getenv("ADMIN_USERNAME")
	if adminUser == "" {
		adminUser = "admin"
	}

	adminPass := os.Getenv("ADMIN_PASSWORD")
	if adminPass == "" {
		adminPass = "admin"
	}

	return &Config{
		OllamaBaseURL:        ollamaURL,
		ClassifierModel:      "llama3.2:3b",
		ThinkingModel:        "glm-4.7-flash:q8_0",
		CodingModel:          "qwen3-coder:latest",
		SimpleModel:          "llama3.2:3b",
		DefaultModel:         "qwen3.5:35b",
		ClassificationPrompt: DefaultClassificationPrompt,
		ClassifierTimeoutS:   10,
		CacheTTLS:            300,
		CacheMaxSize:         500,
		Port:                 port,
		DBPath:               dbPath,
		AdminUsername:        adminUser,
		AdminPassword:        adminPass,
	}
}
