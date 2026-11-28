use super::*;

#[derive(Clone, Copy)]
pub(super) enum Format {
    Gpui(gpui::ImageFormat),
    Raster(image::ImageFormat),
    Dib,
    System,
}

impl Format {
    pub(super) fn for_path(path: &str) -> Option<Self> {
        if let Some(format) = image_preview::image_format_for_name(path) {
            return Some(Self::Gpui(format));
        }
        let extension = Path::new(path).extension()?.to_str()?.to_ascii_lowercase();
        match extension.as_str() {
            "avif" | "heic" | "heif" | "jxl" | "jxr" | "wdp" | "hdp" => Some(Self::System),
            "farbfeld" => Some(Self::Raster(image::ImageFormat::Farbfeld)),
            "dib" => Some(Self::Dib),
            _ => image::ImageFormat::from_extension(extension).map(Self::Raster),
        }
    }

    fn decode(
        self,
        bytes: Vec<u8>,
        svg: gpui::SvgRenderer,
    ) -> anyhow::Result<Arc<gpui::RenderImage>> {
        let decoded = match self {
            Self::Gpui(format) => return gpui::Image::from_bytes(format, bytes).to_image_data(svg),
            Self::Raster(format) => image::load_from_memory_with_format(&bytes, format)?,
            Self::Dib if bytes.starts_with(b"BM") => {
                image::load_from_memory_with_format(&bytes, image::ImageFormat::Bmp)?
            }
            Self::Dib => {
                use image::ImageDecoder;
                let mut decoder = image::codecs::bmp::BmpDecoder::new_without_file_header(
                    std::io::Cursor::new(bytes),
                )?;
                decoder.set_limits(image::Limits::default())?;
                image::DynamicImage::from_decoder(decoder)?
            }
            #[cfg(windows)]
            Self::System => return Ok(render_bgra(decode_windows_image(&bytes)?)),
            #[cfg(not(windows))]
            Self::System => anyhow::bail!("image codec unavailable"),
        };
        let mut pixels = decoded.into_rgba8();
        for pixel in pixels.chunks_exact_mut(4) {
            pixel.swap(0, 2);
        }
        Ok(render_bgra(pixels))
    }
}

fn render_bgra(pixels: image::RgbaImage) -> Arc<gpui::RenderImage> {
    Arc::new(gpui::RenderImage::new(vec![image::Frame::new(pixels)]))
}

// AVIF, HEIF and other extension codecs are supplied by Windows Imaging
// Component. Decode the provided daemon bytes even when the daemon is remote.
#[cfg(windows)]
fn decode_windows_image(bytes: &[u8]) -> anyhow::Result<image::RgbaImage> {
    use windows::Win32::Foundation::RPC_E_CHANGED_MODE;
    use windows::Win32::Graphics::Imaging::*;
    use windows::Win32::System::Com::*;

    // The background worker can already belong to either COM apartment.
    let initialized = unsafe { CoInitializeEx(None, COINIT_MULTITHREADED) };
    if initialized.is_err() && initialized != RPC_E_CHANGED_MODE {
        initialized.ok()?;
    }
    let decode = || -> anyhow::Result<image::RgbaImage> {
        // All COM objects, including the stream that borrows bytes, stay on
        // this thread and are released before the backing slice goes away.
        unsafe {
            let factory: IWICImagingFactory =
                CoCreateInstance(&CLSID_WICImagingFactory, None, CLSCTX_INPROC_SERVER)?;
            let stream = factory.CreateStream()?;
            stream.InitializeFromMemory(bytes)?;
            let decoder = factory.CreateDecoderFromStream(
                &stream,
                std::ptr::null(),
                WICDecodeMetadataCacheOnDemand,
            )?;
            let frame = decoder.GetFrame(0)?;
            let source = WICConvertBitmapSource(&GUID_WICPixelFormat32bppBGRA, &frame)?;
            let (mut width, mut height) = (0u32, 0u32);
            source.GetSize(&mut width, &mut height)?;
            let pixel_count = u64::from(width) * u64::from(height);
            let limits = image::Limits::default();
            anyhow::ensure!(
                pixel_count > 0 && pixel_count <= limits.max_alloc.unwrap_or(u64::MAX) / 4,
                "image exceeds decoder limits"
            );
            let mut pixels = image::RgbaImage::new(width, height);
            source.CopyPixels(std::ptr::null(), width * 4, pixels.as_mut())?;
            Ok(pixels)
        }
    };
    let result = decode();
    if initialized.is_ok() {
        unsafe { CoUninitialize() };
    }
    result
}

enum Preview {
    Loading,
    Ready(Arc<gpui::RenderImage>),
    Failed(String),
}

impl Waku {
    pub(super) fn render_workspace_image(
        &self,
        relative_path: &str,
        format: Format,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Div {
        let theme = Theme::current(cx);
        let body = div()
            .flex_1()
            .min_h_0()
            .min_w_0()
            .flex()
            .items_center()
            .justify_center()
            .p(px(12.0))
            .text_size(sp(12.5))
            .text_center()
            .text_color(theme.text_secondary);
        let Some(root) = self.selected_workspace_path() else {
            return body.child(tr!("files.no_project_is_open"));
        };
        // Only the visible preview retains a decoded image. A late reply for
        // a previous file updates its own entity, never the currently open one.
        let key = SharedString::from(format!(
            "workspace-image-{}-{:?}-{:?}-{}",
            self.right_panel_image_revision, self.state.selected_session, root, relative_path
        ));
        let preview = window.use_keyed_state(key, cx, |_, cx: &mut Context<Preview>| {
            let workspace = waku_client::WorkspaceClient::new(self.daemon.client());
            let operation = waku_client::WorkspaceOperation::ReadFile {
                root: root.to_owned(),
                relative_path: relative_path.into(),
                binary: true,
            };
            let svg = cx.svg_renderer();
            let read = cx.background_executor().spawn(async move {
                let bytes = match workspace.request(operation) {
                    Ok(waku_client::WorkspaceResult::BinaryFile { bytes }) => bytes,
                    Ok(waku_client::WorkspaceResult::TextFile { content }) => content.into_bytes(),
                    Ok(_) => anyhow::bail!("the daemon returned an invalid file response"),
                    Err(error) if error.to_string().contains("valid UTF-8") => {
                        anyhow::bail!(tr!("files.image_daemon_update"))
                    }
                    Err(error) => return Err(error),
                };
                format.decode(bytes, svg)
            });
            cx.spawn(async move |preview, cx| {
                let result = read.await;
                let _ = preview.update(cx, |preview, cx| {
                    *preview = match result {
                        Ok(image) => Preview::Ready(image),
                        Err(error) => Preview::Failed(error.to_string()),
                    };
                    cx.notify();
                });
            })
            .detach();
            cx.on_release(|preview, cx| {
                if let Preview::Ready(image) = preview {
                    let image = image.clone();
                    cx.defer(move |cx| cx.drop_image(image, None));
                }
            })
            .detach();
            Preview::Loading
        });
        match preview.read(cx) {
            Preview::Loading => body.child(tr!("files.loading")),
            Preview::Ready(image) => body.child(
                img(image.clone())
                    .size_full()
                    .object_fit(ObjectFit::Contain),
            ),
            Preview::Failed(error) => body
                .flex_col()
                .gap(px(6.0))
                .child(tr!("attachments.preview_unavailable"))
                .child(error.clone()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn workspace_image_previews_decode_chinese_named_png_and_jpeg() {
        let svg = gpui::SvgRenderer::new(Arc::new(()));
        let mut jpeg = Vec::new();
        image::codecs::jpeg::JpegEncoder::new(&mut jpeg)
            .encode_image(&image::RgbImage::from_pixel(3, 2, image::Rgb([180, 0, 20])))
            .unwrap();
        for (name, bytes) in [
            (
                "图像/01-印文想象图-蔡仲平本.PNG",
                include_bytes!("../../website/public/favicon.png").as_slice(),
            ),
            (
                "图像/02-故宫藏紫砂覆斗纽传国玺印面.jpg",
                jpeg.as_slice(),
            ),
        ] {
            let rendered = Format::for_path(name)
                .unwrap()
                .decode(bytes.to_vec(), svg.clone())
                .unwrap();
            assert!(rendered.size(0).width.0 > 0);
        }
        assert!(Format::for_path("chuanguo-yuxi.html").is_none());
    }

    #[cfg(windows)]
    #[test]
    fn workspace_image_windows_codec_reads_daemon_bytes() {
        let bytes = include_bytes!("../../website/public/favicon.png");
        let pixels = decode_windows_image(bytes).unwrap();
        let expected = image::load_from_memory(bytes).unwrap().into_rgba8();
        assert_eq!(pixels.dimensions(), expected.dimensions());
        let ([b, g, r, a], reference) = (pixels.get_pixel(1, 1).0, expected.get_pixel(1, 1).0);
        assert_eq!([r, g, b, a], reference);
    }
}
