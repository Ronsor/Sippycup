package audiosocket_test

import (
	"bytes"
	"fmt"
	"log"

	"github.com/ronsor/sippycup/audiosocket"
)

func ExampleParseIdentity() {
	identity, err := audiosocket.ParseIdentity(
		[]byte(`{"uuid":"550e8400-e29b-41d4-a716-446655440000","destination":"1234"}`),
		true,
	)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(string(identity.JSON))
	// Output: {"uuid":"550e8400-e29b-41d4-a716-446655440000","destination":"1234"}
}

func ExampleWritePacket() {
	var wire bytes.Buffer
	err := audiosocket.WritePacket(&wire, audiosocket.Packet{
		Kind:    audiosocket.KindDTMF,
		Payload: []byte{'5'},
	})
	if err != nil {
		log.Fatal(err)
	}

	packet, err := audiosocket.ReadPacket(&wire, audiosocket.MaxPayload)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("kind=%#x digit=%s\n", packet.Kind, packet.Payload)
	// Output: kind=0x3 digit=5
}
