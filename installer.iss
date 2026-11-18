; Inno Setup 6 script for Twincxt.
; Built by build.ps1, or manually:  ISCC.exe /DAppVersion=1.0.0 installer.iss

#ifndef AppVersion
  #define AppVersion "1.0.0"
#endif

#define AppName "Twincxt"
; Keep this GUID the same forever so later versions find this install.
#define AppGuid "{B7A3E54C-2F61-4D8E-9C1A-7E0F3D5B6A28}"
#define AppExe "Twincxt.exe"
#define AppPublisher "Twincxt"
; Must match appUserModelID / protocolName in main.go.
#define AppUserModelID "Twincxt.App"
#define Protocol "twincxt"

[Setup]
AppId={#StringChange(AppGuid, "{", "{{")}
AppName={#AppName}
AppVersion={#AppVersion}
AppVerName={#AppName} {#AppVersion}
AppPublisher={#AppPublisher}
VersionInfoVersion={#AppVersion}
VersionInfoDescription={#AppName} Setup

; Per-user install (no admin prompt); "install for all users" stays available.
PrivilegesRequired=lowest
PrivilegesRequiredOverridesAllowed=dialog
DefaultDirName={autopf}\{#AppName}
DisableProgramGroupPage=yes
DisableDirPage=auto

ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0

OutputDir=dist
OutputBaseFilename=Twincxt-Setup-{#AppVersion}
SetupIconFile=icon.ico
UninstallDisplayIcon={app}\{#AppExe}
UninstallDisplayName={#AppName}
WizardStyle=modern
Compression=lzma2/ultra64
SolidCompression=yes

; Close a running Twincxt (Restart Manager) when upgrading or uninstalling.
CloseApplications=force
RestartApplications=no

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"

[Files]
Source: "build\{#AppExe}"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{autoprograms}\{#AppName}"; Filename: "{app}\{#AppExe}"; AppUserModelID: "{#AppUserModelID}"
Name: "{autodesktop}\{#AppName}"; Filename: "{app}\{#AppExe}"; AppUserModelID: "{#AppUserModelID}"; Tasks: desktopicon

[Registry]
; twincxt: links let notification clicks open the right service.
Root: HKCU; Subkey: "Software\Classes\{#Protocol}"; ValueType: string; ValueName: ""; ValueData: "URL:{#AppName}"; Flags: uninsdeletekey
Root: HKCU; Subkey: "Software\Classes\{#Protocol}"; ValueType: string; ValueName: "URL Protocol"; ValueData: ""
Root: HKCU; Subkey: "Software\Classes\{#Protocol}\DefaultIcon"; ValueType: string; ValueName: ""; ValueData: "{app}\{#AppExe},0"
Root: HKCU; Subkey: "Software\Classes\{#Protocol}\shell\open\command"; ValueType: string; ValueName: ""; ValueData: """{app}\{#AppExe}"" ""%1"""
; Remove the "open at sign-in" entry the app may have created.
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: none; ValueName: "{#AppName}"; Flags: uninsdeletevalue

[Run]
Filename: "{app}\{#AppExe}"; Description: "{cm:LaunchProgram,{#AppName}}"; Flags: nowait postinstall skipifsilent
; After an update started from inside Twincxt (silent install), open it again
; as the signed-in user, not as administrator.
Filename: "{app}\{#AppExe}"; Flags: nowait runasoriginaluser; Check: WizardSilent

[Code]
// ---------------------------------------------------------------------------
// Maintenance mode: when Twincxt is already installed, Setup does not just
// overwrite it. It shows what is installed and offers, depending on versions:
//   newer setup  -> Update   / Uninstall
//   same version -> Repair   / Uninstall
//   older setup  -> Downgrade / Uninstall
// Update, Repair and Downgrade keep the install folder, shortcuts choices,
// logins and settings. Uninstall hands over to the existing uninstaller.
// ---------------------------------------------------------------------------

const
  UninstallKey = 'Software\Microsoft\Windows\CurrentVersion\Uninstall\{#AppGuid}_is1';
  ModeInstall = 0;
  ModeUpdate = 1;
  ModeRepair = 2;
  ModeDowngrade = 3;

var
  ModePage: TInputOptionWizardPage;
  Installed: Boolean;
  InstalledVersion, InstalledDir, Uninstaller: String;
  Mode: Integer;
  HandedToUninstaller: Boolean;

function ReadInstall(Root: Integer): Boolean;
begin
  Result := RegQueryStringValue(Root, UninstallKey, 'DisplayVersion', InstalledVersion);
  if Result then
  begin
    RegQueryStringValue(Root, UninstallKey, 'InstallLocation', InstalledDir);
    RegQueryStringValue(Root, UninstallKey, 'UninstallString', Uninstaller);
  end;
end;

function FindInstall: Boolean;
begin
  Result := ReadInstall(HKCU) or ReadInstall(HKLM64) or ReadInstall(HKLM32);
end;

// Takes the next number off a dotted version string ("1.10.2" -> 1, "10.2").
function NextVersionPart(var S: String): Integer;
var
  P: Integer;
begin
  P := Pos('.', S);
  if P = 0 then
  begin
    Result := StrToIntDef(Trim(S), 0);
    S := '';
  end else
  begin
    Result := StrToIntDef(Trim(Copy(S, 1, P - 1)), 0);
    Delete(S, 1, P);
  end;
end;

// 1 if A is newer than B, -1 if older, 0 if equal.
function CompareVersions(A, B: String): Integer;
var
  I, X, Y: Integer;
begin
  Result := 0;
  for I := 1 to 4 do
  begin
    X := NextVersionPart(A);
    Y := NextVersionPart(B);
    if X > Y then begin Result := 1; Exit; end;
    if X < Y then begin Result := -1; Exit; end;
  end;
end;

procedure InitializeWizard;
var
  Cmp: Integer;
  Info: String;
begin
  Mode := ModeInstall;
  Installed := FindInstall;
  if not Installed then
    Exit;

  Cmp := CompareVersions('{#AppVersion}', InstalledVersion);
  if Cmp > 0 then Mode := ModeUpdate
  else if Cmp = 0 then Mode := ModeRepair
  else Mode := ModeDowngrade;

  Info := '{#AppName} ' + InstalledVersion + ' is installed in ' + RemoveBackslash(InstalledDir) + '.';
  case Mode of
    ModeUpdate:
      ModePage := CreateInputOptionPage(wpWelcome, '{#AppName} is already installed',
        'Update to version {#AppVersion} or remove it.',
        Info + #13#10#13#10 + 'Your WhatsApp and Telegram logins and your settings are kept when updating.',
        True, False);
    ModeRepair:
      ModePage := CreateInputOptionPage(wpWelcome, '{#AppName} is already installed',
        'This version is already installed. Repair it or remove it.',
        Info + #13#10#13#10 + 'Repair reinstalls the program files and shortcuts. Your logins and settings are kept.',
        True, False);
  else
      ModePage := CreateInputOptionPage(wpWelcome, 'A newer version of {#AppName} is installed',
        'This setup contains an older version ({#AppVersion}).',
        Info + #13#10#13#10 + 'Installing an older version over a newer one is not recommended.',
        True, False);
  end;

  case Mode of
    ModeUpdate: ModePage.Add('&Update to version {#AppVersion}');
    ModeRepair: ModePage.Add('&Repair {#AppName} {#AppVersion}');
  else
    ModePage.Add('&Replace with older version {#AppVersion}');
  end;
  ModePage.Add('U&ninstall {#AppName}');
  ModePage.SelectedValueIndex := 0;
end;

function Uninstalling: Boolean;
begin
  Result := Installed and (ModePage.SelectedValueIndex = 1);
end;

function NextButtonClick(CurPageID: Integer): Boolean;
var
  Code: Integer;
begin
  Result := True;
  if not Installed then
    Exit;
  if (CurPageID = ModePage.ID) and Uninstalling then
  begin
    // ShellExec (not Exec) so Windows can ask for admin rights when the
    // existing install is for all users.
    if not ShellExec('', RemoveQuotes(Uninstaller), '', '', SW_SHOW, ewNoWait, Code) then
    begin
      MsgBox('The uninstaller could not be started (' + SysErrorMessage(Code) + ').' + #13#10 +
             'Remove {#AppName} from Settings > Apps instead.', mbError, MB_OK);
      Result := False;
      Exit;
    end;
    HandedToUninstaller := True;
    WizardForm.Close;
    Result := False;
  end;
end;

procedure CancelButtonClick(CurPageID: Integer; var Cancel, Confirm: Boolean);
begin
  if HandedToUninstaller then
  begin
    Cancel := True;
    Confirm := False; // no "Exit Setup?" question: the uninstaller took over
  end;
end;

function ShouldSkipPage(PageID: Integer): Boolean;
begin
  // Update / repair / downgrade reuse the existing folder and tasks.
  Result := Installed and ((PageID = wpSelectDir) or (PageID = wpSelectTasks));
end;

function ModeVerb: String;
begin
  case Mode of
    ModeUpdate: Result := 'Update';
    ModeRepair: Result := 'Repair';
    ModeDowngrade: Result := 'Replace';
  else
    Result := 'Install';
  end;
end;

function UpdateReadyMemo(Space, NewLine, MemoUserInfoInfo, MemoDirInfo, MemoTypeInfo,
  MemoComponentsInfo, MemoGroupInfo, MemoTasksInfo: String): String;
begin
  case Mode of
    ModeUpdate: Result := 'Update:' + NewLine + Space + '{#AppName} ' + InstalledVersion + ' -> {#AppVersion}';
    ModeRepair: Result := 'Repair:' + NewLine + Space + '{#AppName} {#AppVersion}';
    ModeDowngrade: Result := 'Replace:' + NewLine + Space + '{#AppName} ' + InstalledVersion + ' -> {#AppVersion} (older)';
  else
    Result := '';
  end;
  if Result <> '' then
    Result := Result + NewLine + NewLine;
  Result := Result + MemoDirInfo;
  if MemoTasksInfo <> '' then
    Result := Result + NewLine + NewLine + MemoTasksInfo;
  if Mode <> ModeInstall then
    Result := Result + NewLine + NewLine + 'Logins and settings are kept.';
end;

procedure CurPageChanged(CurPageID: Integer);
begin
  if Mode = ModeInstall then
    Exit;
  if CurPageID = wpReady then
  begin
    WizardForm.NextButton.Caption := '&' + ModeVerb;
    WizardForm.ReadyLabel.Caption := 'Setup is ready to ' + Lowercase(ModeVerb) + ' {#AppName}. ' +
      'If {#AppName} is running it will be closed first.';
  end
  else if CurPageID = wpFinished then
  begin
    case Mode of
      ModeUpdate: WizardForm.FinishedHeadingLabel.Caption := '{#AppName} has been updated';
      ModeRepair: WizardForm.FinishedHeadingLabel.Caption := '{#AppName} has been repaired';
    else
      WizardForm.FinishedHeadingLabel.Caption := '{#AppName} {#AppVersion} has been installed';
    end;
  end;
end;

// On uninstall, ask whether to also remove the saved logins and settings.
// Default is to keep them, so reinstalling does not require logging in again.
procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  DataDir: String;
begin
  if CurUninstallStep = usPostUninstall then
  begin
    DataDir := ExpandConstant('{userappdata}\{#AppName}');
    if DirExists(DataDir) and not UninstallSilent then
    begin
      if MsgBox('Also delete your WhatsApp and Telegram logins and {#AppName} settings?' + #13#10 + #13#10 +
                'Choose No to stay logged in if you install {#AppName} again.',
                mbConfirmation, MB_YESNO or MB_DEFBUTTON2) = IDYES then
        DelTree(DataDir, True, True, True);
    end;
  end;
end;
