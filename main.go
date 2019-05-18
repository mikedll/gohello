package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"io"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"github.com/qor/render"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
)

type UserApiResponse struct {
	Login       string  `json:"login"`
}

var sBootstrap template.HTML

var isProduction bool

func stateStr() (string, error) {
	c := 30
	b := make([]byte, c)
	_, err := rand.Read(b)
	if err != nil {
		return "", errors.New("couldn't make random str: " + err.Error())
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func main() {
	isProduction = false

	flag.Parse()

	var addr string = ":8081"
	port := os.Getenv("PORT")
	appEnv := os.Getenv("APP_ENV")

	// Going to use this to determine production environment...LOL!
	if appEnv == "production" || port != "" {
		isProduction = true
		addr = fmt.Sprintf(":%s", port)
	}

	openDbForProject(isProduction)
	defer closeDbForProject()

	if flag.NArg() > 0 {
		if flag.Arg(0) == "schema" {
			err := makeSchema()
			if err != nil {
				log.Println("failed to create schema.")
				return
			}

			fmt.Println("created schema.")
		} else if flag.Arg(0) == "sample" {
			err := makeGistFiles()
			if err != nil {
				log.Println("failed to make sample gists", err)
				return
			}
			log.Println("sample gists created.")
		} else if flag.Arg(0) == "empty" {
			err := emptyDb()
			if err != nil {
				log.Println("failed to empty db", err)
				return
			}

			log.Println("emptied database.")
		}
		return
	}

	gists := getGistFiles()
	gistsJson, err := json.Marshal(gists)
	if err != nil {
		log.Println("unable to find gists: ", err)
		gistsJson = []byte{}
	}

	Render := render.New(&render.Config{
		ViewPaths:     []string{},
		DefaultLayout: "",
		FuncMapMaker:  nil,
	})

	root := func(w http.ResponseWriter, req *http.Request) {
		sBootstrap = template.HTML(string(gistsJson))
		ctx := map[string]template.HTML{"Bootstrap": sBootstrap}
		Render.Execute("index", ctx, req, w)
	}

	search := func(w http.ResponseWriter, req *http.Request) {
		// search db for json
		snippets := searchGistFiles(req.URL.Query().Get("q"))
		snippetsJson, err := json.Marshal(snippets)
		if err != nil {
			log.Println("error while marshalling snippets: ", err)
			snippetsJson = []byte{}
			// StatusInternalServerError
			// write "Error while marshalling snippets.
			return
		}

		w.Header().Add("Content-Type", "application/json")
		w.Write(snippetsJson)
		return
	}

	oauth2Conf := &oauth2.Config{
		ClientID:     os.Getenv("GITHUB_CLIENT_ID"),
		ClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		Scopes:       []string{},
		Endpoint: github.Endpoint,
	}

	StateCookieName := "OAuth2-Github-State"
	oauth2Github := func(w http.ResponseWriter, req *http.Request) {
		stateStr, err := stateStr()
		if err != nil {
			log.Println("Unable to generate state string for oauth redirect.")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		stateCookie := http.Cookie{Name: StateCookieName, Value: stateStr, MaxAge: 60 * 15}
		http.SetCookie(w, &stateCookie)

		w.Header().Add("Content-Type", "text/html")

		url := oauth2Conf.AuthCodeURL(stateStr, oauth2.AccessTypeOffline)
		fmt.Println("Redirecting to ", url)
		http.Redirect(w, req, url, http.StatusFound)
	}

  oauth2GithubCallback := func(w http.ResponseWriter, req *http.Request) {
		writeError := func(msg string, errorNum int) {
			w.Header().Add("Content-Type", "text/html")		
			w.WriteHeader(errorNum)
			w.Write([]byte("<br/>" + msg))
		}

		writeInteralServerError := func(msg string) {
			writeError(msg, http.StatusInternalServerError)
		}
		
		cookies := req.Cookies()

		var stateCookieVal string
		for _, cookie := range cookies {
			if cookie.Name == StateCookieName {
				stateCookieVal = cookie.Value
			}
		}		
		code := req.URL.Query().Get("code")
		stateInUrl := req.URL.Query().Get("state")

		if(stateInUrl == stateCookieVal) {
			token, err := oauth2Conf.Exchange(oauth2.NoContext, code)
			if err != nil {
				writeInteralServerError("Could not get access token from code, error: " + err.Error())
				return
			}

			// Access token obtained.
			client := oauth2Conf.Client(oauth2.NoContext, token)

			userUrl := "https://api.github.com/user"
			response, err := client.Get(userUrl)
			if err != nil {
				writeInteralServerError("Error fetching user information at " + userUrl)
				return
			}

			// UserApiResponse
			responseBody := []byte{}
			buf := make([]byte, 1024)			
			var n int
			n, err = response.Body.Read(buf)
			responseBody = append(responseBody, buf[0:n]...)
			for err == nil {
				n, err = response.Body.Read(buf)
				responseBody = append(responseBody, buf[0:n]...)
			}
			
			if err != io.EOF {
				writeInteralServerError("Unable to read response from Github.")
				return
			}

			userApiResponse := UserApiResponse{}
			json.Unmarshal(responseBody, &userApiResponse)
			
			userQueried := User{}
			err = findUserByLogin(userApiResponse.Login, &userQueried)
			if err != nil {
				writeInteralServerError("Unable to query for user: " + err.Error())
				return
			}

			userQueried.AccessToken = token.AccessToken
			userQueried.TokenExpiry = token.Expiry
			
			if(dbConn.NewRecord(userQueried)) {
				userQueried.Username = userApiResponse.Login
				
				dbConn.Create(&userQueried)
				if err = dbConn.Error; err != nil {
					writeInteralServerError("Unable to create user: " + err.Error())
					return
				}
			} else {
				dbConn.Save(&userQueried)
				if err = dbConn.Error; err != nil {
					writeInteralServerError("Unable to save user: " + err.Error())
				}
			}

			w.Header().Add("Content-Type", "text/html")
			w.Write([]byte("Saved user."))
		} else {
			writeError("OAuth2 state variables did not match.", http.StatusBadRequest)
		}
		fmt.Println("Received callback.")
	}
	
	defaultHandler := func(w http.ResponseWriter, req *http.Request) {
		file := "index.jsx"
		err := sendFile(file, w)
		if err != nil {
			log.Println("Unable to serve static file: ", file)
		}
	}

	fmt.Println("Starting server...")
	http.Handle("/", http.HandlerFunc(root))

	http.Handle("/oauth/github", http.HandlerFunc(oauth2Github))
	http.Handle("/oauth/github/callback", http.HandlerFunc(oauth2GithubCallback))
	http.Handle("/api/gists/search", http.HandlerFunc(search))
  http.Handle("/index.jsx", http.HandlerFunc(defaultHandler))

	err = http.ListenAndServe(addr, nil)
	if err != nil {
		log.Fatal("ListenAndServe:", err)
	}
}
