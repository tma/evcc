CREATE TABLE `settings` (
    `key` text
  , `value` text
  , PRIMARY KEY(`key`)
);

INSERT INTO `settings`(key, value) VALUES ('batteryDischargeControl', 'true');
