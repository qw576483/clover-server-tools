@echo off
cd /D "%~dp0..\mysql"
bin\mysqld --defaults-file=my.ini --standalone
