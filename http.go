package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
)

// HTTPSDPServer starts a HTTP Server that consumes SDPs
func HTTPSDPServer() chan string {
	port := flag.Int("port", 8080, "http server port")
	flag.Parse()

	sdpChan := make(chan string)
	http.HandleFunc("/sdp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := ioutil.ReadAll(r.Body)
		sdpChan <- string(body)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if _, err := fmt.Fprintf(w, <-respChan); err != nil {
			log.Fatal(err)
		}
	})

	//http.HandleFunc("/sdpc", func(w http.ResponseWriter, r *http.Request) {
	//	body, _ := ioutil.ReadAll(r.Body)
	//	sdpChan <- string(body)
	//	w.Header().Set("Access-Control-Allow-Origin", "*")
	//	if _, err := fmt.Fprintf(w, <-respChan); err != nil {
	//		log.Fatal(err)
	//	}
	//})

	http.HandleFunc("/restart", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		restart = true
		if _, err := fmt.Fprintf(w, <-respChan); err != nil {
			log.Fatal(err)
		}
	})

	http.Handle("/", http.FileServer(http.Dir("./client")))

	go func() {
		//err := http.ListenAndServe(":"+strconv.Itoa(*port), nil)
		err := http.ListenAndServeTLS(":"+strconv.Itoa(*port), "./full_chain.pem", "./private.key", nil)
		if err != nil {
			panic(err)
		}
	}()

	return sdpChan
}
