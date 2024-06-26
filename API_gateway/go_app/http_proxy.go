package main

import (
        "database/sql"
        "fmt"
        "log"
        "net/http"
        "net/url"
        "os"
        "strconv"
        "strings"
        "sync"
        "time"

        "github.com/joho/godotenv"
        _ "github.com/go-sql-driver/mysql"
        "github.com/patrickmn/go-cache"
        "github.com/prometheus/client_golang/prometheus"
        "github.com/prometheus/client_golang/prometheus/promhttp"
        "github.com/valyala/fasthttp"
)

var apiCache *cache.Cache

type APIUsage struct {
        Count      int64
        LastUpdate time.Time
}

var (
        usageCache = cache.New(24*time.Hour, 30*time.Minute) // Initialize cache with default expiration time and cleanup interval
)

var (
        metricRequestsAPI = prometheus.NewCounterVec(
                prometheus.CounterOpts{
                        Name: "requests_by_api_key",
                        Help: "Number of HTTP requests by API key, organization, organization ID, chain, and status.",
                }, []string{"api_key", "org", "org_id", "chain", "status"},
        )

        metricAPICache = prometheus.NewCounterVec(
                prometheus.CounterOpts{
                        Name: "cache_hits",
                        Help: "Number of calls with cached API key.",
                }, []string{"state"},
        )

        requestsTotal = prometheus.NewCounterVec(
                prometheus.CounterOpts{
                        Name: "http_requests_total",
                        Help: "Total number of HTTP requests.",
                }, []string{"status_code"},
        )
)

// Define a mutex map to store mutexes for each API key
var usageMutexMap sync.Map

func getUsage(apiKey string) *APIUsage {
        // Retrieve the usage for the API key from the cache
        usagePtr, found := usageCache.Get(apiKey)
        if !found {
                return nil
        }
        return usagePtr.(*APIUsage)
}

func setUsage(apiKey string, usage *APIUsage, expire bool) {
        // Update the entry in the cache
        if expire {
            usageCache.Set(apiKey, usage, 24*time.Hour)
        } else {
            usageCache.Set(apiKey, usage, cache.DefaultExpiration)
        }
}

func getUsageMutex(apiKey string) *sync.Mutex {
        // Retrieve or create a mutex for the specified API key
        mutex, _ := usageMutexMap.LoadOrStore(apiKey, &sync.Mutex{})
        return mutex.(*sync.Mutex)
}

func incrementAPIUsage(apiKey string, limit int) bool {
    // Retrieve the mutex for the specified API key
    usageMutex := getUsageMutex(apiKey)

    // Lock the mutex to ensure exclusive access to the usage value for this API key
    usageMutex.Lock()
    defer usageMutex.Unlock()

    // Load the usage for the API key
    usage := getUsage(apiKey)
    if usage == nil {
        // Initialize usage if not found
        usage = &APIUsage{Count: 1, LastUpdate: time.Now()}
        setUsage(apiKey, usage, usage.Count == 1) // If count was 1, then it's an initialization
    } else {
        // Increment the usage count
        if limit != 0 && usage.Count >= int64(limit) {
            return false
        }
        usage.Count++
    }

    return true
}

func main() {
    // Print out the information at the start of the application
    fmt.Println("Welcome to the Liquify API Gateway!")
    fmt.Println("This gateway is developed by Liquify LTD.")
    fmt.Println("For any inquiries, please contact contact@liquify.io.")
    
    // Load environment variables from .env file
    errEnv := godotenv.Load()
    if errEnv != nil {
        log.Fatalf("Error loading .env file: %s", errEnv)
    }

    // Initialize Prometheus metrics
    initPrometheusMetrics()

    // Initialize API cache
    apiCache = cache.New(1*time.Hour, 1*time.Hour)

    // Start FastHTTP server to handle requests
    go startFastHTTPServer()

    // Expose Prometheus metrics endpoint
    startPrometheusServer()

    // Wait indefinitely
    select {}
}

// initPrometheusMetrics initializes Prometheus metrics
func initPrometheusMetrics() {
    prometheus.MustRegister(metricRequestsAPI)
    prometheus.MustRegister(metricAPICache)
    prometheus.MustRegister(requestsTotal)
}

// startFastHTTPServer starts the FastHTTP server
func startFastHTTPServer() {
    errEnv := godotenv.Load()
    if errEnv != nil {
            log.Fatalf("Error loading .env file: %s", errEnv)
    }

    // Retrieve environment variables
    dbPassword := os.Getenv("DB_PASSWORD")
    dbUser := os.Getenv("DB_USER")
    dbHost := os.Getenv("DB_HOST")
    dbPort := os.Getenv("DB_PORT")
    dbDatabaseName := os.Getenv("DB_NAME")
    proxyHost := os.Getenv("PROXY_HOST")
    proxyPort := os.Getenv("PROXY_PORT")

    chainMap := make(map[string][]string)
    keys := []string{"eth","fuse","polygon","solana","bsc","base","arb","dfk","klaytn"}

    for _, key := range keys {
        value := os.Getenv(key)
        if value != "" {
            chainMap[key] = strings.Split(value, ",")
        }
    }
   

    fmt.Println(chainMap)
    // Define the request handler function
    requestHandler := func(ctx *fasthttp.RequestCtx) {
        // Set the headers after calling proxy request as they will get overwritten otherwise (as we copy the headers from the upstream)
        setCORSHeaders := func() {
            if len(ctx.Response.Header.Peek("Access-Control-Allow-Origin")) == 0 {
                ctx.Response.Header.Set("Access-Control-Allow-Origin", "*")
            }
            
            if len(ctx.Response.Header.Peek("Access-Control-Allow-Methods")) == 0 {
                ctx.Response.Header.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
            }
    
            if len(ctx.Response.Header.Peek("Access-Control-Allow-Headers")) == 0 {
                ctx.Response.Header.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
            }
        }
        
        apiKey, path ,err := extractAPIKeyAndPath(ctx)
        if err != nil || apiKey == "" {
            log.Printf("invalid path: %s", path)
            ctx.Error("Forbidden", fasthttp.StatusForbidden)
            return
        }

        // Check if API key exists in cache
        if cacheEntry, found := apiCache.Get(apiKey); found {
            handleCachedAPIKey(ctx, apiKey, cacheEntry.(map[string]interface{}), proxyHost, proxyPort, chainMap)
            setCORSHeaders()
            return
        }

        // Handle API key not found in cache
        handleAPIKeyNotFound(ctx, apiKey, proxyHost, proxyPort, dbUser, dbPassword, dbHost, dbPort, dbDatabaseName, chainMap)
        setCORSHeaders()
    }

    // Start the FastHTTP server on port 80
    if err := fasthttp.ListenAndServe(":80", requestHandler); err != nil {
        log.Fatalf("Error in ListenAndServe: %s", err)
    }
}

// startPrometheusServer starts the Prometheus metrics server
func startPrometheusServer() {
    http.Handle("/metrics", promhttp.Handler())
    if err := http.ListenAndServe(":9100", nil); err != nil {
        log.Fatalf("Error starting Prometheus server: %s", err)
    }
}

// extractAPIKeyAndPath extracts API key and path from request URI
func extractAPIKeyAndPath(ctx *fasthttp.RequestCtx) (string, string, error) {
    uri := string(ctx.RequestURI())
    parsedURI, err := url.Parse(uri)
    if err != nil {
        return "", "", err
    }
    path := parsedURI.Path
    apiKey := extractAPIKey(path)
    return apiKey, path, nil
}

// handleCachedAPIKey handles requests with cached API key
func handleCachedAPIKey(ctx *fasthttp.RequestCtx, apiKey string, keyData map[string]interface{}, proxyHost string, proxyPort string, chainMap map[string][]string) {
    // Check if all required keys exist in the keyData map
    requiredKeys := []string{"limit", "chain", "org", "org_id"}
    for _, key := range requiredKeys {
        if _, ok := keyData[key]; !ok {
            log.Printf("Key '%s' not found in keyData", key)
            return
        }
    }

    // Convert the "limit" value to an int
    limit, ok := keyData["limit"].(int)
    if !ok {
        log.Println("Value associated with 'limit' key is not of type int")
        return
    }

    // Proceed with the request handling
    if !incrementAPIUsage(apiKey, limit) {
        apiCache.Delete(apiKey)
        ctx.Error("Slow down you have hit your daily request limit", fasthttp.StatusTooManyRequests)
        return
    }

    proxyRequest(ctx, &ctx.Request, proxyHost, proxyPort, keyData["chain"].(string),chainMap)
    metricRequestsAPI.WithLabelValues(apiKey, keyData["org"].(string), keyData["org_id"].(string), keyData["chain"].(string), strconv.Itoa(ctx.Response.StatusCode())).Inc()
    metricAPICache.WithLabelValues("HIT").Inc()
}

// handleAPIKeyNotFound handles requests with API key not found
func handleAPIKeyNotFound(ctx *fasthttp.RequestCtx, apiKey string, proxyHost string, proxyPort string, dbUser string, dbPassword string, dbHost string, dbPort string, dbDatabaseName string, chainMap map[string][]string) {

        db, err := sql.Open("mysql", dbUser+":"+dbPassword+"@tcp("+dbHost+":"+dbPort+")/"+dbDatabaseName)
                if err != nil {
                    log.Printf("Error opening database connection: %s", err)
                    ctx.Error("Internal server error", fasthttp.StatusInternalServerError)
                }
                defer db.Close()


    var chain, org string
    var limit, orgID int
    stmt, err := db.Prepare("SELECT chain_name, org_name, `limit`, org_id FROM api_keys WHERE api_key = ?")
    if err != nil {
        log.Fatal(err)
    }
    defer stmt.Close()
    row := stmt.QueryRow(apiKey)
    err = row.Scan(&chain, &org, &limit, &orgID)
    if err != nil {
        if err == sql.ErrNoRows {
            ctx.Error("Invalid API key", fasthttp.StatusForbidden)
            metricAPICache.WithLabelValues("INVALID").Inc()
        } else {
            ctx.Error("Internal server error", fasthttp.StatusInternalServerError)
        }
        return
    }

    if !incrementAPIUsage(apiKey, limit) {
        ctx.Error("Slow down you have hit your daily request limit", fasthttp.StatusTooManyRequests)
        return
    }

    // Cache API key data
    apiCache.Set(apiKey, map[string]interface{}{
        "chain": chain, "org": org, "limit": limit, "org_id": strconv.Itoa(orgID),
    }, 1*time.Hour)

    // Proceed with proxy logic
    proxyRequest(ctx, &ctx.Request, proxyHost, proxyPort, chain, chainMap)
    // Increment API requests metric
    metricRequestsAPI.WithLabelValues(apiKey, org, strconv.Itoa(orgID), chain, strconv.Itoa(ctx.Response.StatusCode())).Inc()
    metricAPICache.WithLabelValues("MISS").Inc()
}

// extractAPIKey extracts API key from the query string
func extractAPIKey(queryString string) string {
    parts := strings.Split(queryString, "=")
    if len(parts) < 2 || parts[0] != "/api" {
        return ""
    }
    return parts[1]
}

func extractAdditionalPath(queryString string) string {
    parts := strings.Split(queryString, "/")
    if len(parts) > 2 {
		// parts[0] will be an empty string because the string starts with '/'
		remainingParts := parts[2:]
		reconstructedPath := "/" + strings.Join(remainingParts, "/")
		
        return reconstructedPath
	} else {
		return ""
	}
}

// Function to proxy the request to the backend server
func proxyRequest(ctx *fasthttp.RequestCtx, req *fasthttp.Request, host string, port string, chain string, chainMap map[string][]string) {
        // Get the input path from the request context
        path := extractAdditionalPath(string(ctx.Path()))

        // Create a new HTTP client
        client := &fasthttp.Client{}
        maxRetries := 3

        if chainCode, ok := chainMap[chain]; ok {
            if len(chainCode) != 0 {
                for attempt := 0; attempt <= maxRetries; attempt++ {
                    uri := ""
                    if attempt < len(chainCode) {
                        uri = chainCode[attempt] + path
                    } else {
                        uri = chainCode[0] + path
                    }
                    req.SetRequestURI(uri)

                    // Perform the request to the backend server
                    backendResp := fasthttp.AcquireResponse()
                    defer fasthttp.ReleaseResponse(backendResp)

                    if err := client.Do(req, backendResp); err != nil {
                            log.Printf("Failed proxy too: %s", uri)
                            requestsTotal.WithLabelValues("502").Inc()
                            ctx.Error(fmt.Sprintf("Error proxying request: %s", err), fasthttp.StatusBadGateway)
                            continue
                    }

                    // Set the response headers and body from the backend response
                    backendResp.Header.CopyTo(&ctx.Response.Header)
                    ctx.Response.SetBody(backendResp.Body())

                    // Increment Prometheus metrics
                    requestsTotal.WithLabelValues(fmt.Sprintf("%d", ctx.Response.StatusCode())).Inc()
                    
                    if backendResp.StatusCode() == fasthttp.StatusOK {
                        return
                    }
                    log.Printf("Failed proxy too: %s", uri)
                }
            } else {
                ctx.Error(fmt.Sprintf("No endpoint found"), fasthttp.StatusBadGateway)
                requestsTotal.WithLabelValues("502").Inc()
                return
            }
        } else {
                ctx.Error(fmt.Sprintf("Chain does not exist in chainMap"), fasthttp.StatusBadGateway)
                requestsTotal.WithLabelValues("502").Inc()
                return
        }
}