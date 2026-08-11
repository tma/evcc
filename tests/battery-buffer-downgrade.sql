CREATE TABLE `settings` (
    `key` text
  , `value` text
  , PRIMARY KEY(`key`)
);

INSERT INTO `settings`(key, value) VALUES ('batteryReserveSoc', '80');
INSERT INTO `settings`(key, value) VALUES ('batterySolarSupport', 'true');
INSERT INTO `settings`(key, value) VALUES ('bufferSoc', '100');
INSERT INTO `settings`(key, value) VALUES ('bufferStartSoc', '0');
