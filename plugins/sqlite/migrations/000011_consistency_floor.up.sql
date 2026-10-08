-- The highest consistency time handed out, kept ahead of use so that a C
-- returned before a restart stays at or below every later C and stamp even
-- if the wall clock stepped back across the restart.
CREATE TABLE consistency_floor (
    id     INTEGER PRIMARY KEY CHECK (id = 1),
    micros INTEGER NOT NULL
);
INSERT INTO consistency_floor (id, micros) VALUES (1, 0);
