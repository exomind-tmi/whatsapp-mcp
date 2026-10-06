package wa

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"

	"go.mau.fi/libsignal/ecc"
	"go.mau.fi/libsignal/keys/identity"
	"go.mau.fi/libsignal/keys/prekey"
	signalproto "go.mau.fi/libsignal/protocol"
	"go.mau.fi/libsignal/session"
	"go.mau.fi/libsignal/util/optional"
	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

// contact is another device that can write to the account: a device of its own
// in store.db, with the keys to start a Signal session with the account's.
func (r *rx) contact(t *testing.T, jid types.JID) *store.Device {
	t.Helper()
	d := r.m.store.NewDevice()
	saveDevice(t, d, jid)
	return d
}

// stanza is the message stanza that WhatsApp delivers to cli when from sends it
// the message: encrypted for its device, as a first message of a session is (a
// pkmsg, which holds the prekey bundle's material and nothing of ours needs to
// be fetched).
func stanza(t *testing.T, from *store.Device, cli *whatsmeow.Client, id string, msg *waE2E.Message) *waBinary.Node {
	t.Helper()
	ctx := context.Background()
	to := cli.Store
	toJID := *to.ID
	addr := toJID.SignalAddress()
	builder := session.NewBuilderFromSignal(from, addr, store.SignalProtobufSerializer)
	bundle := prekey.NewBundle(to.RegistrationID, uint32(toJID.Device), optional.NewEmptyUint32(), to.SignedPreKey.KeyID,
		nil, ecc.NewDjbECPublicKey(*to.SignedPreKey.Pub), *to.SignedPreKey.Signature,
		identity.NewKey(ecc.NewDjbECPublicKey(*to.IdentityKey.Pub)))
	if err := builder.ProcessBundle(ctx, bundle); err != nil {
		t.Fatal(err)
	}
	padded := append(marshaled(t, msg), bytes.Repeat([]byte{5}, 5)...) // whatsmeow's padding: n bytes of n
	ct, err := session.NewCipher(builder, addr).Encrypt(ctx, padded)
	if err != nil {
		t.Fatal(err)
	}
	if ct.Type() != signalproto.PREKEY_TYPE {
		t.Fatalf("the first message of a session is of type %d, not a prekey message", ct.Type())
	}
	return &waBinary.Node{
		Tag: "message",
		Attrs: waBinary.Attrs{
			"from": *from.ID, "id": id, "t": strconv.FormatInt(t0.Unix(), 10), "type": "text", "notify": "Bob",
		},
		Content: []waBinary.Node{{Tag: "enc", Attrs: waBinary.Attrs{"v": "2", "type": "pkmsg"}, Content: ct.Serialize()}},
	}
}

// buffered counts the rows of whatsmeow's event buffer, and those that still hold
// the plaintext of a message.
func (r *rx) buffered(t *testing.T) (rows, withPlaintext int) {
	t.Helper()
	err := r.m.store.db.QueryRow(`SELECT count(*), count(plaintext) FROM whatsmeow_event_buffer`).Scan(&rows, &withPlaintext)
	if err != nil {
		t.Fatal(err)
	}
	return rows, withPlaintext
}

// TestDecryptedEventBufferKeepsAMessageTheArchiveRefused runs real encrypted
// stanzas through whatsmeow's own decryption into our handler, to show why the
// buffer is on. A message whose handler fails comes again, for it was not
// acknowledged; decrypting it a second time is not possible, as the key it was
// read with is spent, and only the buffer has it, until the handler says it
// is done. With the buffer off the same failure loses the message.
func TestDecryptedEventBufferKeepsAMessageTheArchiveRefused(t *testing.T) {
	r := newRx(t, refuseSQL)
	cli := r.cli("personal")
	if !cli.EnableDecryptedEventBuffer {
		t.Fatal("the client does not buffer what it decrypts")
	}
	bob := r.contact(t, types.NewADJID(bobPN, 0, 5))
	ctx := context.Background()
	handle := func(n *waBinary.Node) { cli.DangerousInternals().HandleEncryptedMessage(ctx, n) }

	t.Run("buffer on", func(t *testing.T) {
		node := stanza(t, bob, cli, "M1", text("SECRET-TEXT"))
		r.refuse(t, refuseChats)
		handle(node)
		if n := r.size(t, "personal"); n != [2]int{} {
			t.Fatalf("the archive has %v after a write that was refused", n)
		}
		if rows, kept := r.buffered(t); rows != 1 || kept != 1 {
			t.Errorf("the buffer has %d rows, %d with a plaintext; want the message kept", rows, kept)
		}

		// WhatsApp sends the stanza again, the same bytes, and the archive works now.
		r.allow(t)
		handle(node)
		got := r.msgs(t, "personal", bobPNChat)
		if len(got) != 1 || got[0].ID != "M1" || got[0].Text != "SECRET-TEXT" {
			t.Fatalf("after the stanza came again: %+v", got)
		}
		if rows, kept := r.buffered(t); rows != 1 || kept != 0 {
			t.Errorf("the buffer has %d rows, %d with a plaintext; want the text cleared and the hash kept", rows, kept)
		}

		// And a third time, which the hash drops before our handler sees it.
		handle(node)
		if n := r.size(t, "personal"); n != [2]int{1, 1} {
			t.Errorf("the archive has %v after the stanza came a third time", n)
		}
		if !strings.Contains(r.logs.String(), "already processed") {
			t.Errorf("the third delivery was not dropped as processed:\n%s", r.logs)
		}
	})

	t.Run("buffer off", func(t *testing.T) {
		cli.EnableDecryptedEventBuffer = false
		defer func() { cli.EnableDecryptedEventBuffer = true }()
		node := stanza(t, bob, cli, "M2", text("LOST-TEXT"))
		r.refuse(t, refuseChats)
		handle(node)
		r.allow(t)
		handle(node)
		for _, m := range r.msgs(t, "personal", bobPNChat) {
			if m.ID == "M2" {
				t.Fatal("the message was read twice: the premise of the buffer is wrong, and the test with it")
			}
		}
		if _, kept := r.buffered(t); kept != 0 {
			t.Errorf("the buffer holds %d plaintexts while it is off", kept)
		}
	})
}

// TestDecryptedEventBufferMissesWhenTheLIDBecomesKnown pins a limit of the buffer,
// which is whatsmeow's and not ours to close: an entry is found by the ciphertext and
// by the address the sender encrypts from, which is the phone JID until the store
// knows a LID for it. A message of Bob, known by number alone, that the archive
// refuses is sent again after his LID has become known; it is looked up under the
// other address, not found, decrypted a second time, which the spent key cannot
// do, and acknowledged. The message is lost, and its plaintext stays in the buffer.
//
// If this test fails after whatsmeow is upgraded, the limit is gone: the message is
// stored, and the comment on EnableDecryptedEventBuffer (newClient) is to go with it.
func TestDecryptedEventBufferMissesWhenTheLIDBecomesKnown(t *testing.T) {
	r := newRx(t, refuseSQL)
	cli := r.cli("personal")
	bob := r.contact(t, types.NewADJID(bobPN, 0, 5))
	ctx := context.Background()
	handle := func(n *waBinary.Node) { cli.DangerousInternals().HandleEncryptedMessage(ctx, n) }

	node := stanza(t, bob, cli, "M1", text("SECRET-TEXT"))
	r.refuse(t, refuseChats)
	handle(node)
	if rows, kept := r.buffered(t); rows != 1 || kept != 1 {
		t.Fatalf("the buffer has %d rows, %d with a plaintext; want the message kept", rows, kept)
	}

	// The pair comes, from a group's members or another message, and the archive works.
	if err := cli.Store.LIDs.PutLIDMapping(ctx, lidJID(bobLID), pnJID(bobPN)); err != nil {
		t.Fatal(err)
	}
	r.allow(t)
	handle(node)
	if n := r.size(t, "personal"); n != [2]int{} {
		t.Errorf("the archive has %v: the limit is gone, see the comment of this test", n)
	}
	if !strings.Contains(r.logs.String(), "old counter") {
		t.Errorf("the second delivery was not refused for the spent key:\n%s", r.logs)
	}
	if rows, kept := r.buffered(t); rows != 1 || kept != 1 {
		t.Errorf("the buffer has %d rows, %d with a plaintext; want the first one's, left", rows, kept)
	}
}
