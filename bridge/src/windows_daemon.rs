//! Detached daemon creation must inherit no handles, including non-standard
//! output pipe handles received from PowerShell. Rust's stable Command API
//! currently enables handle inheritance even with all three streams set null.

use std::{ffi::OsStr, ffi::OsString, io, os::windows::ffi::OsStrExt, path::Path, ptr};
use windows_sys::Win32::{
    Foundation::CloseHandle,
    System::Threading::{
        CREATE_NEW_PROCESS_GROUP, CREATE_NO_WINDOW, CreateProcessW, PROCESS_INFORMATION,
        STARTF_USESTDHANDLES, STARTUPINFOW,
    },
};

fn wide(value: &OsStr) -> io::Result<Vec<u16>> {
    let mut encoded: Vec<_> = value.encode_wide().collect();
    if encoded.contains(&0) {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "NUL in daemon argument",
        ));
    }
    encoded.push(0);
    Ok(encoded)
}

fn quote(value: &OsStr, output: &mut Vec<u16>) -> io::Result<()> {
    let encoded = wide(value)?;
    output.push(b'"' as u16);
    let mut slashes = 0;
    for &ch in &encoded[..encoded.len() - 1] {
        if ch == b'\\' as u16 {
            slashes += 1;
            continue;
        }
        let count = if ch == b'"' as u16 {
            slashes * 2 + 1
        } else {
            slashes
        };
        output.extend(std::iter::repeat_n(b'\\' as u16, count));
        output.push(ch);
        slashes = 0;
    }
    output.extend(std::iter::repeat_n(b'\\' as u16, slashes * 2));
    output.push(b'"' as u16);
    Ok(())
}

pub(super) fn spawn(program: &Path, args: &[OsString], cwd: Option<&Path>) -> io::Result<()> {
    let application = wide(program.as_os_str())?;
    let mut command = Vec::new();
    quote(program.as_os_str(), &mut command)?;
    for arg in args {
        command.push(b' ' as u16);
        quote(arg, &mut command)?;
    }
    command.push(0);
    if command.len() > 32767 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "daemon command line exceeds Windows limit",
        ));
    }
    let directory = cwd.map(|path| wide(path.as_os_str())).transpose()?;
    let startup = STARTUPINFOW {
        cb: size_of::<STARTUPINFOW>() as u32,
        // Null stdio is intentional: all protocol traffic uses authenticated
        // IPC. No console or caller pipe may survive into the daemon.
        dwFlags: STARTF_USESTDHANDLES,
        ..Default::default()
    };
    let mut process = PROCESS_INFORMATION::default();
    // SAFETY: all strings are NUL-terminated and alive for the call; the
    // writable command buffer is exclusive. No security attributes or handles
    // are inherited, the environment is inherited unchanged, and both returned
    // handles are closed immediately after successful process creation.
    let created = unsafe {
        CreateProcessW(
            application.as_ptr(),
            command.as_mut_ptr(),
            ptr::null(),
            ptr::null(),
            0,
            CREATE_NEW_PROCESS_GROUP | CREATE_NO_WINDOW,
            ptr::null(),
            directory
                .as_ref()
                .map_or(ptr::null(), |value| value.as_ptr()),
            &startup,
            &mut process,
        )
    };
    if created == 0 {
        return Err(io::Error::last_os_error());
    }
    unsafe {
        CloseHandle(process.hThread);
        CloseHandle(process.hProcess);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn windows_arguments_preserve_quotes_empty_values_and_trailing_slashes() {
        for (argument, expected) in [
            ("", "\"\""),
            ("a b", "\"a b\""),
            ("a\"b", "\"a\\\"b\""),
            ("C:\\folder with spaces\\", "\"C:\\folder with spaces\\\\\""),
            ("a\\\"b", "\"a\\\\\\\"b\""),
            ("目录 α", "\"目录 α\""),
        ] {
            let mut output = Vec::new();
            quote(OsStr::new(argument), &mut output).unwrap();
            assert_eq!(String::from_utf16(&output).unwrap(), expected);
        }
        assert!(quote(OsStr::new("bad\0value"), &mut Vec::new()).is_err());
    }
}
