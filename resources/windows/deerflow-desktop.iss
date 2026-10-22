; DeerFlow-specific installer. Do not use the upstream Waku updater/AppId.
#ifndef AppVersion
  #error AppVersion is required
#endif
#ifndef InstallerAppId
  #define InstallerAppId "A77F56A9-B6EF-42CC-A36D-E80592F4092B"
#endif

[Setup]
AppId={#InstallerAppId}
AppName=DeerFlow Desktop
AppVersion={#AppVersion}
VersionInfoVersion={#AppVersion}
AppPublisher=DeerFlow
DefaultDirName={localappdata}\Programs\DeerFlowDesktop
DefaultGroupName=DeerFlow Desktop
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0.17763
OutputDir={#OutputDir}
OutputBaseFilename={#OutputBaseName}
SetupIconFile={#AppIcon}
UninstallDisplayIcon={app}\deerflow-desktop.exe
UninstallDisplayName=DeerFlow Desktop
LicenseFile={#StageDir}\resources\licenses\Waku-GPL-3.0.txt
InfoBeforeFile={#StageDir}\README.txt
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
DisableProgramGroupPage=yes
UsePreviousAppDir=yes
SetupMutex=DeerFlowDesktopSetup
; Never terminate active agent tasks to replace locked binaries.
CloseApplications=no
RestartApplications=no
Uninstallable=yes

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; Flags: unchecked

[Files]
#include FileList

[Icons]
Name: "{group}\DeerFlow Desktop"; Filename: "{app}\deerflow-desktop.exe"; WorkingDir: "{app}"; AppUserModelID: "app.deerflow.desktop"
Name: "{userdesktop}\DeerFlow Desktop"; Filename: "{app}\deerflow-desktop.exe"; WorkingDir: "{app}"; Tasks: desktopicon; AppUserModelID: "app.deerflow.desktop"

[Run]
Filename: "{app}\deerflow-desktop.exe"; Description: "{cm:LaunchProgram,DeerFlow Desktop}"; WorkingDir: "{app}"; Flags: nowait postinstall skipifsilent

; user-data is created by the app, is never installed, and is never removed.
; No recursive InstallDelete/UninstallDelete entries may be added here.
[Code]
function CheckInstalledProcesses(): String;
var
  Locator, Services, Processes, Process: Variant;
  Index: Integer;
  ExecutablePath, InstallPrefix: String;
begin
  Result := '';
  InstallPrefix := Lowercase(AddBackslash(ExpandConstant('{app}')));
  try
    Locator := CreateOleObject('WbemScripting.SWbemLocator');
    Services := Locator.ConnectServer('', 'root\CIMV2');
    { Restrict WMI enumeration: ItemIndex over all system processes is slow. }
    Processes := Services.ExecQuery('SELECT ExecutablePath FROM Win32_Process WHERE Name = ''waku-daemon.exe'' OR Name LIKE ''deerflow-%.exe''');
    for Index := 0 to Processes.Count - 1 do
    begin
      Process := Processes.ItemIndex(Index);
      if not VarIsNull(Process.ExecutablePath) then
      begin
        ExecutablePath := Process.ExecutablePath;
        ExecutablePath := Lowercase(ExecutablePath);
        if (Pos(InstallPrefix, ExecutablePath) = 1) and
           (Pos('unins', Lowercase(ExtractFileName(ExecutablePath))) <> 1) then
        begin
          Result := 'DeerFlow is still running from this folder. Finish active tasks, close Desktop and stop its background services, then retry. No programs have been stopped.';
          Exit;
        end;
      end;
    end;
  except
    Result := 'Cannot check running processes (Windows Management Instrumentation). No files were changed. Restore WMI access and retry.';
  end;
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  Result := CheckInstalledProcesses();
end;

function InitializeUninstall(): Boolean;
var
  Reason: String;
begin
  Reason := CheckInstalledProcesses();
  Result := Reason = '';
  if not Result then
    SuppressibleMsgBox(Reason, mbError, MB_OK, IDOK);
end;
