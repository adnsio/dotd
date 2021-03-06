package server

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/adnsio/dotd/pkg/roundrobin"
	"github.com/rs/zerolog/log"
	"golang.org/x/net/dns/dnsmessage"
)

type Server struct {
	udpAddress         *net.UDPAddr
	upstreamRoundRobin *roundrobin.RoundRobin
	udpConnection      *net.UDPConn
	blocklist          map[string]bool
	blockRegex         []*regexp.Regexp
	resolve            map[string]string
	httpClient         *http.Client
}

func (s *Server) ListenAndServe() error {
	var err error

	s.udpConnection, err = net.ListenUDP("udp", s.udpAddress)
	if err != nil {
		return fmt.Errorf("udp: %w", err)
	}
	defer s.udpConnection.Close()

	log.Info().Msgf("listening on %s", s.udpAddress.String())

	exit := make(chan bool)

	for i := 0; i < runtime.NumCPU(); i++ {
		// launch a go routine to read data from udp
		go s.readFromUDP()
	}

	<-exit

	return nil
}

func (s *Server) readFromUDP() {
	data := make([]byte, 1024)

	for {
		dataLength, addr, err := s.udpConnection.ReadFromUDP(data)
		if err != nil {
			// log error and continue
			log.Err(fmt.Errorf("udp: %w", err)).Send()

			continue
		}

		// launch a go routine to answer
		go func() {
			// unpack data as dns message
			dnsMessage := &dnsmessage.Message{}

			err := dnsMessage.Unpack(data[:dataLength])
			if err != nil {
				log.Err(fmt.Errorf("dnsmessage: %w", err)).Send()

				return
			}

			if err := s.answerDNSMessage(addr, dnsMessage, data[:dataLength]); err != nil {
				log.Err(err).
					Uint16("id", dnsMessage.ID).
					Send()
			}
		}()
	}
}

func (s *Server) answerDNSMessage(addr *net.UDPAddr, dnsMessage *dnsmessage.Message, data []byte) error {
	question := dnsMessage.Questions[0]

	log.Debug().
		Uint16("id", dnsMessage.ID).
		Str("name", question.Name.String()).
		Str("type", question.Type.String()).
		Msgf("dns question from %s", addr.String())

	// try resolve
	answeredDNSMessage, err := s.answerQuestionWithResolve(dnsMessage.ID, &question)
	if err != nil {
		return err
	}

	if answeredDNSMessage != nil {
		if err := s.writeDNSMessageToUPD(answeredDNSMessage, addr); err != nil {
			return err
		}

		return nil
	}

	// try blocklist
	answeredDNSMessage = s.answerQuestionWithBlocklist(dnsMessage.ID, &question)
	if answeredDNSMessage != nil {
		if err := s.writeDNSMessageToUPD(answeredDNSMessage, addr); err != nil {
			return err
		}

		return nil
	}

	// try blockregex
	answeredDNSMessage = s.answerQuestionWithBlockregex(dnsMessage.ID, &question)
	if answeredDNSMessage != nil {
		if err := s.writeDNSMessageToUPD(answeredDNSMessage, addr); err != nil {
			return err
		}

		return nil
	}

	// forward to upstream
	answerData, err := s.forwardDataToUpstream(dnsMessage.ID, data)
	if err != nil {
		return err
	}

	if err := s.writeDataToUDP(dnsMessage.ID, answerData, addr); err != nil {
		return err
	}

	return nil
}

func (s *Server) writeDNSMessageToUPD(msg *dnsmessage.Message, addr *net.UDPAddr) error {
	msgData, err := msg.Pack()
	if err != nil {
		return fmt.Errorf("dnsmessage: %w", err)
	}

	if err := s.writeDataToUDP(msg.ID, msgData, addr); err != nil {
		return err
	}

	return nil
}

func (s *Server) writeDataToUDP(id uint16, data []byte, addr *net.UDPAddr) error {
	_, err := s.udpConnection.WriteToUDP(data, addr)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}

	log.Debug().
		Uint16("id", id).
		Msg("dns question answered")

	return nil
}

func (s *Server) forwardDataToUpstream(id uint16, data []byte) ([]byte, error) {
	maxAttempts := s.upstreamRoundRobin.Length()

	for i := 0; i < maxAttempts; i++ {
		upstream, err := s.upstreamRoundRobin.Pick()
		if err != nil {
			return nil, fmt.Errorf("roundrobin: %w", err)
		}

		log.Debug().
			Uint16("id", id).
			Int("attempt", i+1).
			Int("maxAttempts", maxAttempts).
			Msgf(`forwarding request to "%s"`, upstream.String())

		dataReader := bytes.NewReader(data)

		req, err := http.NewRequest(http.MethodPost, upstream.String(), dataReader)
		if err != nil {
			return nil, fmt.Errorf("http: %w", err)
		}

		req.Header.Add("content-type", "application/dns-message")
		req.Header.Add("accept", "application/dns-message")

		res, err := s.httpClient.Do(req)
		if err != nil {
			log.Err(fmt.Errorf("http: %w", err)).
				Uint16("id", id).
				Int("attempt", i+1).
				Send()

			continue
		}
		defer res.Body.Close()

		if res.StatusCode != 200 {
			log.Err(fmt.Errorf(`http: invalid status code "%d"`, res.StatusCode)).
				Uint16("id", id).
				Int("attempt", i+1).
				Send()

			continue
		}

		resData, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return nil, fmt.Errorf("io: %w", err)
		}

		return resData, nil
	}

	return nil, errors.New("max attempts reached")
}

func (s *Server) answerQuestionWithBlocklist(id uint16, question *dnsmessage.Question) *dnsmessage.Message {
	name := question.Name.Data[:question.Name.Length-1]

	blocklisted, ok := s.blocklist[string(name)]
	if !ok || !blocklisted {
		return nil
	}

	log.Warn().
		Uint16("id", id).
		Msgf(`"%s" is blocked`, name)

	answeredDNSMessage := &dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:                 id,
			Response:           true,
			RecursionAvailable: true,
			RecursionDesired:   true,
			RCode:              dnsmessage.RCodeNameError,
		},
		Questions: []dnsmessage.Question{
			*question,
		},
	}

	return answeredDNSMessage
}

func (s *Server) answerQuestionWithBlockregex(id uint16, question *dnsmessage.Question) *dnsmessage.Message {
	name := question.Name.Data[:question.Name.Length-1]

	for _, regex := range s.blockRegex {
		if regex.MatchString(string(name)) {
			log.Warn().
				Uint16("id", id).
				Msgf(`"%s" is blocked from regex`, name)

			answeredDNSMessage := &dnsmessage.Message{
				Header: dnsmessage.Header{
					ID:                 id,
					Response:           true,
					RecursionAvailable: true,
					RecursionDesired:   true,
					RCode:              dnsmessage.RCodeNameError,
				},
				Questions: []dnsmessage.Question{
					*question,
				},
			}

			return answeredDNSMessage
		}
	}

	return nil
}

func (s *Server) answerQuestionWithResolve(id uint16, question *dnsmessage.Question) (*dnsmessage.Message, error) {
	name := question.Name.Data[:question.Name.Length-1]

	resolved, ok := s.resolve[string(name)]
	if !ok || resolved == "" {
		// try with wildcard
		nameSlices := strings.Split(string(name), ".")
		if len(nameSlices) < 2 {
			return nil, nil
		}

		nameWildcard := fmt.Sprintf("*.%s.%s", nameSlices[len(nameSlices)-2], nameSlices[len(nameSlices)-1])
		_ = nameWildcard

		resolved, ok = s.resolve[nameWildcard]
		if !ok || resolved == "" {
			return nil, nil
		}
	}

	log.Debug().
		Uint16("id", id).
		Msgf(`resolving "%s"`, name)

	resolvedIP := net.ParseIP(resolved)
	if resolvedIP == nil {
		return nil, fmt.Errorf(`invalid ip address "%s"`, resolved)
	}

	answeredDNSMessage := &dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:                 id,
			Response:           true,
			RecursionAvailable: true,
			RecursionDesired:   true,
		},
		Questions: []dnsmessage.Question{
			*question,
		},
	}

	switch question.Type {
	case dnsmessage.TypeA:
		ip4 := resolvedIP.To4()
		if ip4 == nil {
			return answeredDNSMessage, nil
		}

		var a [4]byte

		copy(a[:], ip4)

		answeredDNSMessage.Answers = []dnsmessage.Resource{
			{
				Header: dnsmessage.ResourceHeader{
					Name:  question.Name,
					Type:  dnsmessage.TypeA,
					Class: dnsmessage.ClassINET,
				},
				Body: &dnsmessage.AResource{
					A: a,
				},
			},
		}
	case dnsmessage.TypeAAAA:
		if resolvedIP.To4() != nil {
			return answeredDNSMessage, nil
		}

		var aaaa [16]byte

		copy(aaaa[:], resolvedIP)

		answeredDNSMessage.Answers = []dnsmessage.Resource{
			{
				Header: dnsmessage.ResourceHeader{
					Name:  question.Name,
					Type:  dnsmessage.TypeAAAA,
					Class: dnsmessage.ClassINET,
				},
				Body: &dnsmessage.AAAAResource{
					AAAA: aaaa,
				},
			},
		}
	default:
		return nil, fmt.Errorf(`invalid question type "%s"`, question.Type)
	}

	return answeredDNSMessage, nil
}

func parseUDPAddress(address string) (*net.UDPAddr, error) {
	host, stringPort, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("net: %w", err)
	}

	port, err := strconv.Atoi(stringPort)
	if err != nil {
		return nil, fmt.Errorf("strconv: %w", err)
	}

	parsedIP := net.ParseIP(host)
	if parsedIP == nil {
		return nil, fmt.Errorf(`"%s" is not a valid ip address`, host)
	}

	return &net.UDPAddr{
		IP:   parsedIP,
		Port: port,
	}, nil
}

func New(
	address string,
	upstreams []string,
	blocklist []string,
	blockregex []string,
	resolve map[string]string,
) (*Server, error) {
	udpAddress, err := parseUDPAddress(address)
	if err != nil {
		return nil, err
	}

	upstreamURLs := make([]*url.URL, 0, len(upstreams))

	for _, upstream := range upstreams {
		upstreamURL, err := url.Parse(upstream)
		if err != nil {
			return nil, fmt.Errorf("url: %w", err)
		}

		upstreamURLs = append(upstreamURLs, upstreamURL)
	}

	upstreamRoundRobin := roundrobin.New(upstreamURLs)

	blocklistMap := make(map[string]bool, len(blocklist))
	for _, blocklistItem := range blocklist {
		blocklistMap[blocklistItem] = true
	}

	compiledBlockregex := make([]*regexp.Regexp, 0, len(blockregex))

	for _, regex := range blockregex {
		compiledRegex, err := regexp.Compile(regex)
		if err != nil {
			return nil, fmt.Errorf("regexp: %w", err)
		}

		compiledBlockregex = append(compiledBlockregex, compiledRegex)
	}

	return &Server{
		udpAddress:         udpAddress,
		upstreamRoundRobin: upstreamRoundRobin,
		blocklist:          blocklistMap,
		resolve:            resolve,
		blockRegex:         compiledBlockregex,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}, nil
}
