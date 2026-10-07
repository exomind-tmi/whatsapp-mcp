package archive

import (
	"errors"
	"fmt"
)

// The PN chat's messages that the LID chat does not have are moved to it (see
// moveMessagesSQL); the rest, which are on both sides, are poured into the LID
// chat's with an upsert, and not dropped with a DELETE of what the move has left: a
// message that is on both sides is a conflict, and the side that loses must give the
// side that wins what it has, or the original that lay under the PN is deleted along
// with it and only the stub that lay under the LID remains. So at a conflict the
// non-empty value wins; the text of the later edit wins; the header (sender, from_me,
// ts) comes from the real message when the LID side is a stub, as it does in Upsert.
// (An INSERT ... SELECT needs a WHERE, or its ON CONFLICT is parsed as the ON of a
// join.) The rows the pour has inserted or updated go through the FTS triggers, and
// the delete that follows takes the PN rows out of the index.
const mergeMessagesSQL = `
INSERT INTO messages(account, chat_jid, msg_id, sender_jid, from_me, ts, text, media_type, media_mime,
  media_name, media_size, media_path, quoted_id, edited_at, revoked_at, raw)
SELECT account, ?3, msg_id, sender_jid, from_me, ts, text, media_type, media_mime,
  media_name, media_size, media_path, quoted_id, edited_at, revoked_at, raw
FROM messages WHERE account = ?1 AND chat_jid = ?2 ORDER BY ts, id
ON CONFLICT(account, chat_jid, msg_id) DO UPDATE SET
  raw = coalesce(messages.raw, excluded.raw),
  media_type = coalesce(messages.media_type, excluded.media_type),
  media_mime = coalesce(messages.media_mime, excluded.media_mime),
  media_name = coalesce(messages.media_name, excluded.media_name),
  media_size = coalesce(messages.media_size, excluded.media_size),
  media_path = coalesce(messages.media_path, excluded.media_path),
  quoted_id = coalesce(messages.quoted_id, excluded.quoted_id),
  text = CASE WHEN coalesce(excluded.edited_at, 0) > coalesce(messages.edited_at, 0) THEN excluded.text
              WHEN messages.edited_at IS NOT NULL THEN messages.text
              ELSE coalesce(messages.text, excluded.text) END,
  edited_at = max(coalesce(messages.edited_at, excluded.edited_at), coalesce(excluded.edited_at, messages.edited_at)),
  revoked_at = coalesce(messages.revoked_at, excluded.revoked_at),
  sender_jid = CASE WHEN messages.raw IS NULL AND excluded.raw IS NOT NULL THEN excluded.sender_jid ELSE messages.sender_jid END,
  from_me = CASE WHEN messages.raw IS NULL AND excluded.raw IS NOT NULL THEN excluded.from_me ELSE messages.from_me END,
  ts = CASE WHEN messages.raw IS NULL AND excluded.raw IS NOT NULL THEN excluded.ts ELSE messages.ts END`

// The messages that are not on both sides are moved, not poured: they keep their ids,
// so that a cursor a client holds into the chat, which is (ts, id), is still a position
// in it after the merge, and a page that goes back from it does not skip the messages of
// the second it ends in. What the move leaves under the PN (the messages that are on both
// sides, which UPDATE OR IGNORE does not touch) is what the statement above pours. The
// index of the text is keyed by id and by nothing else of the row, so a move does not
// touch it.
const moveMessagesSQL = `UPDATE OR IGNORE messages SET chat_jid = ?3 WHERE account = ?1 AND chat_jid = ?2`

// The chat row is poured the same way: the LID row keeps what it has, the PN
// row supplies what it lacks, and the PN-JID itself becomes the LID row's pn.
const mergeChatSQL = `
INSERT INTO chats(account, jid, pn, name, is_group, last_message_ts)
SELECT account, ?3, coalesce(pn, ?2), name, is_group, last_message_ts FROM chats WHERE account = ?1 AND jid = ?2
ON CONFLICT(account, jid) DO UPDATE SET
  pn = coalesce(chats.pn, excluded.pn),
  name = coalesce(chats.name, excluded.name),
  is_group = max(chats.is_group, excluded.is_group),
  last_message_ts = max(coalesce(chats.last_message_ts, excluded.last_message_ts),
                        coalesce(excluded.last_message_ts, chats.last_message_ts))`

// MergeChat folds the chat that was kept under the phone number (fromJID, a
// PN-JID) into the one under the LID (toJID), once WhatsApp has told which
// LID the number is: its messages and its row move over, and what is on both
// sides is combined without losing any of it. Calling it again, or when
// there is nothing under fromJID, changes nothing.
//
// A client's cursor into the chat stays a position in it across a merge: the
// messages that moved keep their ids. (A message that was on both sides is poured
// into the one of the LID chat and takes its id; a cursor at the other is still at the
// right time, but it may be off by the messages of its own second.)
func (t *Tx) MergeChat(account, fromJID, toJID string) error {
	switch {
	case account == "" || fromJID == "" || toJID == "":
		return errors.New("merge chat: account and both jids are required")
	case fromJID == toJID:
		return nil // the DELETEs below would erase the chat that was to be kept
	}
	for _, step := range []struct {
		query string
		args  []any
	}{
		{moveMessagesSQL, []any{account, fromJID, toJID}},
		{mergeMessagesSQL, []any{account, fromJID, toJID}},
		{deleteChatMessagesSQL, []any{account, fromJID}},
		{mergeChatSQL, []any{account, fromJID, toJID}},
		{deleteChatSQL, []any{account, fromJID}},
	} {
		if _, err := t.tx.ExecContext(t.ctx, step.query, step.args...); err != nil {
			return fmt.Errorf("merge chat of account %q: %w", account, err)
		}
	}
	return nil
}

const (
	deleteChatMessagesSQL = `DELETE FROM messages WHERE account = ?1 AND chat_jid = ?2`
	deleteChatSQL         = `DELETE FROM chats WHERE account = ?1 AND jid = ?2`
)
