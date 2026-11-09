-- A deleted workflow keeps its row and versions, so its runs and their events stay readable;
-- deleted_at says when. It leaves the pages and cannot be edited, chosen or run again.
ALTER TABLE workflows ADD COLUMN deleted_at timestamptz;
