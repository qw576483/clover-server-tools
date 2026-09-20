Set ws = CreateObject("WScript.Shell")
here = CreateObject("Scripting.FileSystemObject").GetParentFolderName(WScript.ScriptFullName)
ws.Run "cmd /c " & Chr(34) & here & "\mysql_start.bat" & Chr(34), 0, false
